package networks

import (
	"os"
	"path/filepath"
	"testing"
)

// storeWithFakeIpset подменяет ipset скриптом, который записывает вызовы.
func storeWithFakeIpset(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "ipset")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(dir, "networks.list"))
	s.IpsetBin = bin
	return s, calls
}

func TestAddStoresAndApplies(t *testing.T) {
	s, calls := storeWithFakeIpset(t)

	added, err := s.Add([]string{"91.108.4.0/22", "149.154.160.0/20"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("добавлено %d, ожидалось 2", added)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("в списке %d записей: %v", len(list), list)
	}

	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	// Записи должны добавляться без срока жизни: у таблицы Кваса он сутки.
	if want := "add KVAS_LIST 91.108.4.0/22 timeout 0 -exist"; !contains(string(logged), want) {
		t.Errorf("ожидался вызов %q, получено:\n%s", want, logged)
	}

	// Повторное добавление тех же подсетей ничего не меняет.
	added, err = s.Add([]string{"91.108.4.0/22"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("повторное добавление дало %d новых записей", added)
	}
}

func TestAddRejectsGarbage(t *testing.T) {
	s, _ := storeWithFakeIpset(t)
	for _, bad := range []string{"не подсеть", "999.1.1.0/24", "2001:b28:f23d::/48"} {
		if _, err := s.Add([]string{bad}); err == nil {
			t.Errorf("%q должно отвергаться", bad)
		}
	}
}

func TestRemove(t *testing.T) {
	s, calls := storeWithFakeIpset(t)
	if _, err := s.Add([]string{"91.108.4.0/22", "149.154.160.0/20"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("91.108.4.0/22"); err != nil {
		t.Fatal(err)
	}

	list, _ := s.List()
	if len(list) != 1 || list[0] != "149.154.160.0/20" {
		t.Errorf("после удаления список: %v", list)
	}
	logged, _ := os.ReadFile(calls)
	if !contains(string(logged), "del KVAS_LIST 91.108.4.0/22") {
		t.Errorf("подсеть должна убираться и из таблицы:\n%s", logged)
	}

	// Удаление отсутствующей записи не считается ошибкой.
	if err := s.Remove("8.8.8.0/24"); err != nil {
		t.Errorf("удаление отсутствующей подсети: %v", err)
	}
}

func TestApplyReturnsCountAndSurvivesEmptyList(t *testing.T) {
	s, _ := storeWithFakeIpset(t)

	// Пустой список — ipset не вызывается, ошибки нет.
	if n, err := s.Apply(); err != nil || n != 0 {
		t.Errorf("для пустого списка получено n=%d, err=%v", n, err)
	}

	if _, err := s.Add([]string{"185.76.151.0/24"}); err != nil {
		t.Fatal(err)
	}
	// Повторное применение нужно после `kvas init`: он пересоздаёт таблицу.
	if n, err := s.Apply(); err != nil || n != 1 {
		t.Errorf("получено n=%d, err=%v", n, err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
