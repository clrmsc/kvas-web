// Package selfupd проверяет и ставит обновления самого пакета Квас.
//
// Обновление заменяет в том числе бинарник веб-интерфейса, поэтому процесс,
// который его запустил, будет остановлен на середине. Установка отдаётся
// отдельному процессу, отвязанному от сервиса: иначе opkg успел бы
// распаковать половину файлов и умереть вместе с нами.
package selfupd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	repo        = "clrmsc/kvas-web"
	baseURL     = "https://github.com/" + repo + "/releases/latest/download/"
	maxPackage  = 32 << 20
	packageName = "kvas"
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
func Latest(ctx context.Context) (Release, error) {
	asset, err := assetForArch()
	if err != nil {
		return Release{}, err
	}

	version, err := fetchVersion(ctx, baseURL+"version")
	if err != nil {
		return Release{}, err
	}
	return Release{Version: version, Asset: asset, URL: baseURL + asset}, nil
}

// fetchVersion читает файл с версией, выложенный рядом с пакетами.
func fetchVersion(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kvas-web")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("не удалось проверить обновление: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("сервер обновлений ответил кодом %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	// Когда файла нет, GitHub отдаёт страницу с ошибкой — версией её считать
	// нельзя, иначе интерфейс предложит «обновиться» на кусок html.
	if version == "" || len(version) > 64 || strings.ContainsAny(version, " \t<>") {
		return "", fmt.Errorf("сервер обновлений вернул неожиданный ответ")
	}
	return version, nil
}

// NeedsUpdate сообщает, отличается ли выложенная версия от установленной.
func NeedsUpdate(installed, latest string) bool {
	i, l := strings.TrimSpace(installed), strings.TrimSpace(latest)
	return i != "" && l != "" && i != l
}

// Download скачивает пакет во временный файл рядом с указанным каталогом.
func Download(ctx context.Context, rel Release, dir string, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kvas-web")

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
