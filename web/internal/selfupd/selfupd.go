// Package selfupd проверяет и ставит обновления самого пакета Квас.
//
// Обновление заменяет в том числе бинарник веб-интерфейса, поэтому процесс,
// который его запустил, будет остановлен на середине. Установка отдаётся
// отдельному процессу, отвязанному от сервиса: иначе opkg успел бы
// распаковать половину файлов и умереть вместе с нами.
package selfupd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	repo       = "clrmsc/kvas-web"
	releaseAPI = "https://api.github.com/repos/" + repo + "/releases/latest"
	maxPackage = 32 << 20
	// versionPrefix — начало имени файла-метки с версией сборки.
	versionPrefix = "version-"
	packageName   = "kvas"
)

// Release — сведения об опубликованном обновлении.
type Release struct {
	Version string `json:"version"` // версия пакета, например 1.1.9_beta-10-41
	Asset   string `json:"asset"`   // имя ipk под нашу архитектуру
	URL     string `json:"-"`
}

// assetForArch возвращает имя пакета под ту же архитектуру, на которой
// собран сервис.
func assetForArch() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "kvas-aarch64.ipk", nil
	case "arm":
		return "kvas-armv7.ipk", nil
	case "mipsle":
		return "kvas-mipsle.ipk", nil
	case "mips":
		return "kvas-mips.ipk", nil
	}
	return "", fmt.Errorf("для архитектуры %s пакеты не собираются", runtime.GOARCH)
}

// Installed возвращает версию установленного пакета по базе opkg.
func Installed() string {
	data, err := os.ReadFile("/opt/lib/opkg/status")
	if err != nil {
		return ""
	}
	inPkg := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "Package: "):
			inPkg = strings.TrimSpace(strings.TrimPrefix(line, "Package: ")) == packageName
		case inPkg && strings.HasPrefix(line, "Version: "):
			return strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
		}
	}
	return ""
}

// Latest узнаёт версию, выложенную в последнем релизе.
//
// Версия берётся из имени файла-метки (version-1.1.9_beta-10-43), а список
// файлов запрашивается через API: раздача релизов какое-то время отдаёт
// прежнее содержимое по тому же имени, и свежая сборка выглядела бы как
// «обновлений нет».
func Latest(ctx context.Context) (Release, error) {
	assetName, err := assetForArch()
	if err != nil {
		return Release{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kvas-web")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("не удалось проверить обновление: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("сервер обновлений ответил кодом %d", resp.StatusCode)
	}

	var payload struct {
		Assets []asset `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return Release{}, err
	}

	version, url := pickFromAssets(payload.Assets, assetName)
	rel := Release{Asset: assetName, Version: version, URL: url}
	if rel.Version == "" {
		return rel, fmt.Errorf("в релизе нет метки версии")
	}
	if rel.URL == "" {
		return rel, fmt.Errorf("в релизе нет пакета %s", assetName)
	}
	return rel, nil
}

// asset — файл, приложенный к релизу.
type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// pickFromAssets достаёт версию из имени файла-метки и ссылку на пакет
// под нужную архитектуру.
func pickFromAssets(assets []asset, want string) (version, url string) {
	for _, a := range assets {
		switch {
		case strings.HasPrefix(a.Name, versionPrefix):
			version = strings.TrimPrefix(a.Name, versionPrefix)
		case a.Name == want:
			url = a.URL
		}
	}
	return version, url
}

// NeedsUpdate сообщает, отличается ли выложенная версия от установленной.
func NeedsUpdate(installed, latest string) bool {
	i, l := strings.TrimSpace(installed), strings.TrimSpace(latest)
	return i != "" && l != "" && i != l
}

// Download скачивает пакет во временный файл рядом с указанным каталогом.
// cacheBustURL добавляет к ссылке метку версии: имя файла в релизе
// постоянное, и раздача GitHub ещё несколько часов отдаёт по нему прежнюю
// сборку. С меткой каждый выпуск запрашивается по своему адресу.
func cacheBustURL(rel Release) string {
	if rel.Version == "" {
		return rel.URL
	}
	sep := "?"
	if strings.Contains(rel.URL, "?") {
		sep = "&"
	}
	return rel.URL + sep + "v=" + url.QueryEscape(rel.Version)
}

func Download(ctx context.Context, rel Release, dir string, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cacheBustURL(rel), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kvas-web")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("не удалось скачать пакет: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("пакет отдан с кодом %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, "kvas-update-*.ipk")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	total := resp.ContentLength
	written, err := copyWithProgress(tmp, io.LimitReader(resp.Body, maxPackage), total, progress)
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	// Пакет заведомо больше мегабайта: так отсеивается страница с ошибкой,
	// отданная вместо файла.
	if written < 1<<20 {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("скачан неполный файл (%d байт)", written)
	}
	return tmp.Name(), nil
}

// PackageVersion читает версию из скачанного пакета. Формат ipk у Entware —
// tar.gz с control.tar.gz внутри, а в нём файл control с полем Version.
func PackageVersion(path string) (string, error) {
	outer, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer outer.Close()

	control, err := findInTarGz(outer, "control.tar.gz")
	if err != nil {
		return "", fmt.Errorf("это не похоже на пакет ipk: %w", err)
	}
	text, err := findInTarGz(bytes.NewReader(control), "control")
	if err != nil {
		return "", fmt.Errorf("в пакете нет описания: %w", err)
	}

	for _, line := range strings.Split(string(text), "\n") {
		if v, ok := strings.CutPrefix(line, "Version:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("в описании пакета нет версии")
}

// findInTarGz достаёт из tar.gz файл с указанным именем, не обращая
// внимания на ведущее «./».
func findInTarGz(r io.Reader, name string) ([]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("файл %s не найден", name)
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(hdr.Name, "./") != name {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxPackage))
	}
}

// Install запускает установку пакета отдельным процессом и возвращает
// управление сразу: сервис будет остановлен в ходе установки.
//
// Вывод opkg пишется в logPath — по нему видно, чем закончилось обновление,
// когда интерфейс снова поднимется.
func Install(pkgPath, logPath string) error {
	script := fmt.Sprintf(`
sleep 2
echo "=== %s: установка %s ==="
opkg install --force-reinstall --force-downgrade %q
rc=$?
rm -f %q
echo "=== opkg завершился с кодом $rc ==="
# Сервис перезапускает postinst, но подстрахуемся: без веб-интерфейса
# роутер останется без управления из браузера.
sleep 3
/opt/etc/init.d/S99kvasweb check >/dev/null 2>&1 || /opt/etc/init.d/S99kvasweb start
`, time.Now().Format("2006-01-02 15:04"), pkgPath, pkgPath, pkgPath)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Новая группа процессов: остановка сервиса не должна убить установку.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Процесс живёт своей жизнью — ждать его мы не можем и не должны.
	go func() { _ = cmd.Wait() }()
	return nil
}

func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress func(done, total int64)) (int64, error) {
	buf := make([]byte, 128<<10)
	var done int64
	last := time.Now()

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			if progress != nil && time.Since(last) > time.Second {
				progress(done, total)
				last = time.Now()
			}
		}
		if err == io.EOF {
			if progress != nil {
				progress(done, total)
			}
			return done, nil
		}
		if err != nil {
			return done, err
		}
	}
}
