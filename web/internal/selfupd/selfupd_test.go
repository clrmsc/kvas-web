package selfupd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNeedsUpdate(t *testing.T) {
	cases := []struct {
		installed, latest string
		want              bool
	}{
		{"1.1.9_beta-10-41", "1.1.9_beta-10-42", true},
		{"1.1.9_beta-10-42", "1.1.9_beta-10-42", false},
		{"", "1.1.9_beta-10-42", false}, // версия неизвестна — не навязываем
		{"1.1.9_beta-10-41", "", false}, // не узнали последнюю — молчим
	}
	for _, c := range cases {
		if got := NeedsUpdate(c.installed, c.latest); got != c.want {
			t.Errorf("NeedsUpdate(%q, %q) = %v, ожидалось %v", c.installed, c.latest, got, c.want)
		}
	}
}

func TestParseReleaseAssets(t *testing.T) {
	assets := []asset{
		{Name: "kvas-aarch64.ipk", URL: "https://example/kvas-aarch64.ipk"},
		{Name: "version-1.1.9_beta-10-43"},
		{Name: "kvasweb-aarch64", URL: "https://example/kvasweb"},
	}

	version, url := pickFromAssets(assets, "kvas-aarch64.ipk")
	if version != "1.1.9_beta-10-43" {
		t.Errorf("версия разобрана как %q", version)
	}
	if url != "https://example/kvas-aarch64.ipk" {
		t.Errorf("ссылка на пакет разобрана как %q", url)
	}

	// Метки версии нет — обновляться не на что.
	if v, _ := pickFromAssets(assets[:1], "kvas-aarch64.ipk"); v != "" {
		t.Errorf("без метки версия должна быть пустой, получено %q", v)
	}
	// Пакета под нашу архитектуру нет.
	if _, u := pickFromAssets(assets, "kvas-mipsle.ipk"); u != "" {
		t.Errorf("ссылка не должна находиться, получено %q", u)
	}
}

func TestDownloadRejectsSmallFile(t *testing.T) {
	// Вместо пакета может прийти страница с ошибкой — она заведомо мала.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Not Found")
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := Download(context.Background(), Release{URL: srv.URL}, dir, nil)
	if err == nil {
		t.Fatal("неполный файл должен отвергаться")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("после отказа не должно оставаться файлов, осталось %d", len(left))
	}
}

func TestInstallRunsDetached(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "kvas.ipk")
	if err := os.WriteFile(pkg, []byte("пакет"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "update.log")

	// Установка идёт отдельным процессом: вызов возвращается сразу, а
	// работа продолжается сама.
	start := time.Now()
	if err := Install(pkg, logPath); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("вызов должен возвращаться сразу, занял %s", elapsed)
	}

	// Скрипт стартует с паузой; дожидаемся первых строк в журнале.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), "установка") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("установка не записала ничего в журнал")
}

func TestPackageVersion(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "kvas.ipk")
	buildIPK(t, pkg, "Package: kvas\nVersion: 1.1.9_beta-10-44\nArchitecture: all\n")

	got, err := PackageVersion(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.1.9_beta-10-44" {
		t.Errorf("получено %q", got)
	}
}

func TestPackageVersionRejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	notPkg := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(notPkg, []byte("просто текст"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackageVersion(notPkg); err == nil {
		t.Error("посторонний файл не должен разбираться как пакет")
	}
}

// buildIPK собирает минимальный пакет: tar.gz с control.tar.gz внутри —
// ровно тот формат, что использует Entware.
func buildIPK(t *testing.T, path, control string) {
	t.Helper()

	var inner bytes.Buffer
	izw := gzip.NewWriter(&inner)
	itw := tar.NewWriter(izw)
	if err := itw.WriteHeader(&tar.Header{Name: "./control", Size: int64(len(control)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	itw.Write([]byte(control))
	itw.Close()
	izw.Close()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, body := range map[string][]byte{
		"./debian-binary":  []byte("2.0\n"),
		"./control.tar.gz": inner.Bytes(),
		"./data.tar.gz":    []byte("не важно"),
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	zw.Close()
}
