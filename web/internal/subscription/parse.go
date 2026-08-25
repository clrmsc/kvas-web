// Package subscription загружает подписку VPN-провайдера и разбирает её
// на список серверов, пригодных для проверки и подключения.
package subscription

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Server — один сервер из подписки.
type Server struct {
	Name    string `json:"name"`    // подпись из ссылки, например «🇳🇱 Netherlands - Amsterdam»
	Address string `json:"address"` // хост сервера
	Port    int    `json:"port"`
	ID      string `json:"id"` // UUID пользователя

	Network     string `json:"network"`     // tcp, ws, grpc, xhttp
	Security    string `json:"security"`    // reality, tls, none
	SNI         string `json:"sni"`         //
	PublicKey   string `json:"public_key"`  // pbk, только для reality
	ShortID     string `json:"short_id"`    // sid
	Fingerprint string `json:"fingerprint"` // fp
	Flow        string `json:"flow"`
	SpiderX     string `json:"spider_x"`
	Path        string `json:"path"`    // для ws/xhttp
	HostHeader  string `json:"host"`    // для ws
	ServiceName string `json:"service"` // для grpc
	Raw         string `json:"-"`       // исходная ссылка; наружу не отдаём
}

// Endpoint — адрес сервера в виде host:port.
func (s Server) Endpoint() string {
	return net.JoinHostPort(s.Address, strconv.Itoa(s.Port))
}

// Key — устойчивый идентификатор сервера: имя может повторяться,
// а пара адрес-порт различает записи подписки.
func (s Server) Key() string { return s.Endpoint() }

// Parse разбирает тело подписки. Поддерживаются оба распространённых
// формата: список ссылок в открытом виде и он же целиком в base64.
// Строки-комментарии (метаданные профиля) пропускаются.
func Parse(body string) ([]Server, error) {
	text := strings.TrimSpace(body)
	if decoded, ok := decodeBase64(text); ok {
		text = decoded
	}

	var servers []Server
	var errs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "vless://") {
			// Подписки часто смешивают протоколы; неподдерживаемые молча
			// пропускаем, чтобы остальные серверы всё равно работали.
			continue
		}
		s, err := ParseVless(line)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		servers = append(servers, s)
	}

	if len(servers) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("не удалось разобрать ни одной ссылки: %s", errs[0])
		}
		return nil, fmt.Errorf("в подписке нет ссылок vless://")
	}
	return servers, nil
}

// ParseVless разбирает одну ссылку вида
// vless://uuid@host:port?type=tcp&security=reality&pbk=...#подпись
func ParseVless(link string) (Server, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Server{}, fmt.Errorf("ссылка не разбирается: %w", err)
	}
	if u.Scheme != "vless" {
		return Server{}, fmt.Errorf("неподдерживаемый протокол %q", u.Scheme)
	}
	if u.User == nil || u.User.Username() == "" {
		return Server{}, fmt.Errorf("в ссылке нет идентификатора пользователя")
	}

	host := u.Hostname()
	if host == "" {
		return Server{}, fmt.Errorf("в ссылке нет адреса сервера")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return Server{}, fmt.Errorf("некорректный порт в ссылке на %s", host)
	}

	q := u.Query()
	s := Server{
		Name:        strings.TrimSpace(u.Fragment),
		Address:     host,
		Port:        port,
		ID:          u.User.Username(),
		Network:     firstNonEmpty(q.Get("type"), "tcp"),
		Security:    firstNonEmpty(q.Get("security"), "none"),
		SNI:         firstNonEmpty(q.Get("sni"), q.Get("host")),
		PublicKey:   q.Get("pbk"),
		ShortID:     q.Get("sid"),
		Fingerprint: firstNonEmpty(q.Get("fp"), "chrome"),
		Flow:        q.Get("flow"),
		SpiderX:     firstNonEmpty(q.Get("spx"), "/"),
		Path:        q.Get("path"),
		HostHeader:  q.Get("host"),
		ServiceName: q.Get("serviceName"),
		Raw:         link,
	}
	if s.Name == "" {
		s.Name = s.Endpoint()
	}
	if s.Security == "reality" && s.PublicKey == "" {
		return Server{}, fmt.Errorf("для %s не указан публичный ключ reality", s.Name)
	}
	return s, nil
}

// decodeBase64 пытается раскодировать подписку целиком. Провайдеры отдают
// её и в открытом виде, и в base64 — иногда без выравнивающих знаков «=».
func decodeBase64(text string) (string, bool) {
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(text)
	if compact == "" || strings.Contains(text, "://") {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(compact); err == nil && strings.Contains(string(raw), "://") {
			return string(raw), true
		}
	}
	return "", false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
