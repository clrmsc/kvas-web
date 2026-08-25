package kvas

import (
	"errors"
	"net/netip"
	"strings"
)

// ErrBadDomain — строка не похожа на доменное имя.
var ErrBadDomain = errors.New("некорректное доменное имя")

// ErrBadIP — строка не является IP-адресом или подсетью.
var ErrBadIP = errors.New("некорректный IP-адрес или подсеть")

// NormalizeDomain приводит домен к нижнему регистру и проверяет его.
// Разрешена ведущая звёздочка: Квас понимает шаблоны вида *.example.com.
func NormalizeDomain(s string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	d = strings.TrimSuffix(d, ".")
	if d == "" || len(d) > 253 {
		return "", ErrBadDomain
	}
	body := strings.TrimPrefix(d, "*.")
	if body == "" || !strings.Contains(body, ".") {
		return "", ErrBadDomain
	}
	for _, label := range strings.Split(body, ".") {
		if label == "" || len(label) > 63 {
			return "", ErrBadDomain
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrBadDomain
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return "", ErrBadDomain
			}
		}
	}
	return d, nil
}

// NormalizeIP проверяет одиночный IPv4-адрес или подсеть в формате CIDR.
// Маршрутизация Кваса работает только с IPv4, поэтому IPv6 отвергаем явно.
func NormalizeIP(s string) (string, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", ErrBadIP
	}
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil || !p.Addr().Is4() {
			return "", ErrBadIP
		}
		return p.Masked().String(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || !a.Is4() {
		return "", ErrBadIP
	}
	return a.String(), nil
}

// NormalizeName проверяет имя закваски (тега) — оно попадает в имя секции
// файла tags.list, поэтому скобки и переводы строк недопустимы.
func NormalizeName(s string) (string, error) {
	n := strings.TrimSpace(s)
	if n == "" || len(n) > 64 {
		return "", errors.New("имя должно быть от 1 до 64 символов")
	}
	if strings.ContainsAny(n, "[]\n\r\t/\\") {
		return "", errors.New("имя содержит недопустимые символы")
	}
	return n, nil
}
