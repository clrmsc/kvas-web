// Package xrayupd обновляет клиент xray с официальных сборок XTLS.
//
// В репозитории Entware xray нередко отстаёт на несколько версий, а
// провайдеры обновляют серверную часть Reality — старый клиент перестаёт
// договариваться с частью серверов и рвёт соединение с сообщением
// «REALITY: received real certificate». Со стороны это выглядит как
// «сервер не работает», поэтому обновление вынесено в интерфейс.
package xrayupd

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	releaseAPI = "https://api.github.com/repos/XTLS/Xray-core/releases/latest"
	// Архив со всеми файлами сборки весит около 20 МБ; больше ждать неоткуда.
	maxArchiveSize = 64 << 20
)

// Release — сведения о последней опубликованной сборке.
type Release struct {
	Version   string `json:"version"` // например v26.3.27
	AssetName string `json:"asset"`   // имя архива под нашу архитектуру
	AssetURL  string `json:"-"`
	DigestURL string `json:"-"`
	Published string `json:"published"`
	Size      int64  `json:"size_bytes"`
}

// assetForArch возвращает имя архива, собранного под ту же архитектуру,
// на которой работает сам сервис: он собран под процессор роутера.
func assetForArch() (string, error) {
	switch runtime.GOARCH {
	case "arm64":
		return "Xray-linux-arm64-v8a.zip", nil
	case "amd64":
		return "Xray-linux-64.zip", nil
	case "386":
		return "Xray-linux-32.zip", nil
	case "arm":
		// Сборка сервиса идёт с GOARM=7, других вариантов не выпускаем.
		return "Xray-linux-arm32-v7a.zip", nil
	case "mipsle":
		return "Xray-linux-mips32le.zip", nil
	case "mips":
		return "Xray-linux-mips32.zip", nil
	}
	return "", fmt.Errorf("для архитектуры %s готовых сборок xray нет", runtime.GOARCH)
}

// Latest узнаёт последнюю версию xray и ссылку на архив под наш процессор.
func Latest(ctx context.Context) (Release, error) {
	want, err := assetForArch()
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
		return Release{}, fmt.Errorf("не удалось узнать версию на GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub ответил кодом %d", resp.StatusCode)
	}

	var payload struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return Release{}, err
	}

	rel := Release{Version: payload.TagName, AssetName: want}
	if len(payload.PublishedAt) >= 10 {
		rel.Published = payload.PublishedAt[:10]
	}
	for _, a := range payload.Assets {
		switch a.Name {
		case want:
			rel.AssetURL, rel.Size = a.URL, a.Size
		case want + ".dgst":
			rel.DigestURL = a.URL
		}
	}
	if rel.AssetURL == "" {
		return rel, fmt.Errorf("в релизе %s нет сборки %s", rel.Version, want)
	}
	return rel, nil
}

// NeedsUpdate сравнивает установленную версию с последней. Версии
// нормализуются: у xray формат «26.3.27», у тега релиза — «v26.3.27».
func NeedsUpdate(installed, latest string) bool {
	i := strings.TrimPrefix(strings.TrimSpace(installed), "v")
	l := strings.TrimPrefix(strings.TrimSpace(latest), "v")
	return i != "" && l != "" && i != l
}

// Download скачивает архив во временный файл и проверяет контрольную сумму,
// если она опубликована. Возвращает путь к файлу.
func Download(ctx context.Context, rel Release, dir string, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	tmp, err := os.CreateTemp(dir, "xray-*.zip")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	req.Header.Set("User-Agent", "kvas-web")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("не удалось скачать архив: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("сервер отдал архив с кодом %d", resp.StatusCode)
	}

	hash := sha256.New()
	written, err := copyWithProgress(io.MultiWriter(tmp, hash),
		io.LimitReader(resp.Body, maxArchiveSize), rel.Size, progress)
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if rel.Size > 0 && written != rel.Size {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("скачано %d байт вместо %d", written, rel.Size)
	}

	if rel.DigestURL != "" {
		want, err := fetchDigest(ctx, rel.DigestURL)
		if err != nil {
			// Отсутствие контрольной суммы не повод ставить непроверенное.
			os.Remove(tmp.Name())
			return "", err
		}
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, want) {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("контрольная сумма не совпала: скачано %s, ожидалось %s", got, want)
		}
	}
	return tmp.Name(), nil
}

// fetchDigest читает файл .dgst и достаёт из него SHA2-256.
func fetchDigest(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kvas-web")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("не удалось получить контрольную сумму: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if _, value, ok := strings.Cut(line, "SHA2-256="); ok {
			return strings.TrimSpace(value), nil
		}
	}
	return "", fmt.Errorf("в файле контрольных сумм нет строки SHA2-256")
}

// ExtractBinary достаёт из архива исполняемый файл xray.
func ExtractBinary(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("архив не открывается: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if filepath.Base(f.Name) != "xray" || f.FileInfo().IsDir() {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		defer src.Close()

		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, io.LimitReader(src, maxArchiveSize)); err != nil {
			return err
		}
		return out.Sync()
	}
	return fmt.Errorf("в архиве нет файла xray")
}

// copyWithProgress копирует данные, сообщая о ходе скачивания.
func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress func(done, total int64)) (int64, error) {
	buf := make([]byte, 128<<10)
	var done int64
	lastReport := time.Now()

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
			// Сообщаем не чаще раза в секунду: на медленном канале иначе
			// в браузер уходит поток бесполезных событий.
			if progress != nil && time.Since(lastReport) > time.Second {
				progress(done, total)
				lastReport = time.Now()
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
