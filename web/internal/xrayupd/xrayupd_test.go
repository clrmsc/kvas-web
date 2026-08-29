package xrayupd

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNeedsUpdate(t *testing.T) {
	cases := []struct {
		installed, latest string
		want              bool
	}{
		{"26.2.6", "v26.3.27", true},
		{"26.3.27", "v26.3.27", false},  // тег с префиксом v — та же версия
		{"v26.3.27", "v26.3.27", false}, //
		{"", "v26.3.27", false},         // версия неизвестна — не навязываем обновление
		{"26.2.6", "", false},           // не узнали последнюю — тоже молчим
	}
	for _, c := range cases {
		if got := NeedsUpdate(c.installed, c.latest); got != c.want {
			t.Errorf("NeedsUpdate(%q, %q) = %v, ожидалось %v", c.installed, c.latest, got, c.want)
		}
	}
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.zip")

	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// В настоящем архиве рядом с бинарником лежат geoip.dat и прочее.
	for name, body := range map[string]string{
		"geoip.dat": "не то",
		"xray":      "#!/bin/sh\necho Xray 26.3.27\n",
		"README.md": "не то",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()

	dest := filepath.Join(dir, "xray")
	if err := ExtractBinary(archive, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#!/bin/sh\necho Xray 26.3.27\n" {
		t.Errorf("извлечён не тот файл: %q", data)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("извлечённый файл должен быть исполняемым")
	}
}

func TestExtractBinaryWithoutXray(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.zip")
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("geoip.dat")
	w.Write([]byte("данные"))
	zw.Close()
	f.Close()

	if err := ExtractBinary(archive, filepath.Join(dir, "xray")); err == nil {
		t.Error("архив без xray должен отвергаться")
	}
}

func TestDownloadChecksDigest(t *testing.T) {
	payload := []byte("содержимое архива")
	sum := sha256.Sum256(payload)

	var digestBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dgst" {
			fmt.Fprint(w, digestBody)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	rel := Release{
		AssetURL:  srv.URL + "/archive",
		DigestURL: srv.URL + "/dgst",
		Size:      int64(len(payload)),
	}

	// Сумма совпадает — файл принимается.
	digestBody = "MD5= 0\nSHA2-256= " + hex.EncodeToString(sum[:]) + "\n"
	path, err := Download(context.Background(), rel, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("файл с верной суммой должен приниматься: %v", err)
	}
	os.Remove(path)

	// Сумма чужая — файл отвергается, мусор за собой не оставляем.
	dir := t.TempDir()
	digestBody = "SHA2-256= " + hex.EncodeToString(make([]byte, 32)) + "\n"
	if _, err := Download(context.Background(), rel, dir, nil); err == nil {
		t.Error("файл с неверной контрольной суммой должен отвергаться")
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Errorf("после отказа временные файлы должны удаляться, осталось: %d", len(left))
	}
}

func TestDownloadReportsProgress(t *testing.T) {
	payload := make([]byte, 300<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	var reported bool
	rel := Release{AssetURL: srv.URL, Size: int64(len(payload))}
	path, err := Download(context.Background(), rel, t.TempDir(), func(done, total int64) {
		if done > 0 && total == int64(len(payload)) {
			reported = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if !reported {
		t.Error("о ходе скачивания должно сообщаться")
	}
}
