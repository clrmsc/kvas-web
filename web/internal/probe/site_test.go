package probe

import "testing"

func TestNormalizeSiteURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"instagram.com", "https://instagram.com/"},
		{"  www.instagram.com  ", "https://www.instagram.com/"},
		{"https://i.instagram.com/api/v1/", "https://i.instagram.com/api/v1/"},
		{"http://example.org/path", "http://example.org/path"},
	}
	for _, c := range cases {
		got, err := NormalizeSiteURL(c.in)
		if err != nil {
			t.Errorf("%q: неожиданная ошибка %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q → %q, ожидалось %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "   ", "ftp://example.com", "localhost", "не адрес"} {
		if got, err := NormalizeSiteURL(bad); err == nil {
			t.Errorf("%q принято как %q, ожидалась ошибка", bad, got)
		}
	}
}

// Длинные технические ошибки в таблице не нужны — там важно лишь то,
// открылся адрес или нет.
func TestShortSiteError(t *testing.T) {
	cases := map[string]string{
		"Get \"https://x\": context deadline exceeded":              "нет ответа",
		"read tcp 127.0.0.1:1 -> 2: read: connection reset by peer": "соединение сброшено",
		"dial tcp 1.2.3.4:443: connect: connection refused":         "соединение отклонено",
		// Так выглядит обёртка TunnelCheck — в таблице от неё толку нет.
		"через туннель нет доступа в интернет: Get \"https://x\": EOF": "нет ответа",
		"задержку через туннель измерить не удалось":                   "нет ответа",
	}
	for in, want := range cases {
		if got := shortSiteError(errString(in)); got != want {
			t.Errorf("%q → %q, ожидалось %q", in, got, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
