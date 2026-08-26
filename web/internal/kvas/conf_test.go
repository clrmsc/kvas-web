package kvas

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestConfSetPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kvas.conf")
	initial := "APP_VERSION=1.1.9\nroute_full_ip=192.168.1.5\nDNS_DEFAULT=127.0.0.1#9753\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	c := Conf{Path: path}
	if err := c.Set("route_full_ip", "192.168.1.5+192.168.1.7"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("NEW_KEY", "value"); err != nil {
		t.Fatal(err)
	}

	all, err := c.GetAll()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"APP_VERSION":   "1.1.9",
		"route_full_ip": "192.168.1.5+192.168.1.7",
		"DNS_DEFAULT":   "127.0.0.1#9753",
		"NEW_KEY":       "value",
	}
	for k, v := range want {
		if all[k] != v {
			t.Errorf("ключ %s = %q, ожидалось %q", k, all[k], v)
		}
	}
	if len(all) != len(want) {
		t.Errorf("в файле %d ключей, ожидалось %d: %v", len(all), len(want), all)
	}
}

func TestConfSetCollapsesDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kvas.conf")
	// Shell-версия веб-морды дописывала ключи в конец, не удаляя старые.
	if err := os.WriteFile(path, []byte("key=old\nother=1\nkey=older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Conf{Path: path}).Set("key", "new"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "key=new\nother=1\n" {
		t.Errorf("получено %q", got)
	}
}

func TestReadTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tags.list")
	body := "# комментарий\n[Видео]\nyoutube.com\nvimeo.com\n\n[Соцсети]\nfacebook.com\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tags, err := ReadTags(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("получено %d заквасок, ожидалось 2", len(tags))
	}
	// Порядок алфавитный: «Видео» перед «Соцсети».
	if tags[0].Name != "Видео" || len(tags[0].Domains) != 2 {
		t.Errorf("первая закваска разобрана неверно: %+v", tags[0])
	}
	if tags[1].Name != "Соцсети" || tags[1].Domains[0] != "facebook.com" {
		t.Errorf("вторая закваска разобрана неверно: %+v", tags[1])
	}
}

func TestReadListMissingFile(t *testing.T) {
	list, err := ReadList(filepath.Join(t.TempDir(), "нет-файла"))
	if err != nil {
		t.Fatalf("отсутствующий файл должен читаться как пустой список: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("ожидался пустой список, получено %v", list)
	}
}

func TestPortListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if !PortListening(port) {
		t.Errorf("занятый порт %d определён как свободный", port)
	}

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	if PortListening(freePort) {
		t.Errorf("свободный порт %d определён как занятый", freePort)
	}
}

func TestSetupFinished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kvas.conf")

	cases := map[string]bool{
		"SETUP_FINISHED=true\n":  true,
		"SETUP_FINISHED=yes\n":   true,
		"SETUP_FINISHED=\n":      false,
		"SETUP_FINISHED=false\n": false,
		"INFACE_ENT=t2s21\n":     false,
	}
	for body, want := range cases {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := SetupFinished(path); got != want {
			t.Errorf("для %q получено %v, ожидалось %v", body, got, want)
		}
	}

	if SetupFinished(filepath.Join(dir, "нет-файла")) {
		t.Error("без файла настройка не может считаться завершённой")
	}
}
