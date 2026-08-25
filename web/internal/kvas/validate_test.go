package kvas

import "testing"

func TestNormalizeDomain(t *testing.T) {
	ok := map[string]string{
		"Example.COM":      "example.com",
		"  youtube.com  ":  "youtube.com",
		"sub.example.com.": "sub.example.com",
		"*.example.com":    "*.example.com",
		"my-site.co.uk":    "my-site.co.uk",
	}
	for in, want := range ok {
		got, err := NormalizeDomain(in)
		if err != nil {
			t.Errorf("NormalizeDomain(%q) вернул ошибку: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDomain(%q) = %q, ожидалось %q", in, got, want)
		}
	}

	bad := []string{
		"", "localhost", "example", "-bad.com", "bad-.com",
		"exa mple.com", "example.com; rm -rf /", "example.com\nevil.com",
		"a..com", "*.", "$(id).com",
	}
	for _, in := range bad {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("NormalizeDomain(%q) = %q, ожидалась ошибка", in, got)
		}
	}
}

func TestNormalizeIP(t *testing.T) {
	ok := map[string]string{
		"192.168.1.10":   "192.168.1.10",
		" 10.0.0.1 ":     "10.0.0.1",
		"192.168.1.0/24": "192.168.1.0/24",
		"192.168.1.5/24": "192.168.1.0/24", // адрес приводится к границе сети
	}
	for in, want := range ok {
		got, err := NormalizeIP(in)
		if err != nil {
			t.Errorf("NormalizeIP(%q) вернул ошибку: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeIP(%q) = %q, ожидалось %q", in, got, want)
		}
	}

	for _, in := range []string{"", "не ip", "999.1.1.1", "::1", "2001:db8::/32", "192.168.1.1; reboot"} {
		if _, err := NormalizeIP(in); err == nil {
			t.Errorf("NormalizeIP(%q): ожидалась ошибка", in)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	if _, err := NormalizeName("Соцсети"); err != nil {
		t.Errorf("корректное имя отклонено: %v", err)
	}
	for _, in := range []string{"", "  ", "[секция]", "имя\nдругое", "a/b"} {
		if _, err := NormalizeName(in); err == nil {
			t.Errorf("NormalizeName(%q): ожидалась ошибка", in)
		}
	}
}
