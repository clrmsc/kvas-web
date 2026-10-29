package probe

// site.go — проверка одного адреса через серверы подписки. Нужна, когда
// туннель исправен, а конкретный сервис через него не открывается: часть
// серверов не пускают, например, к Meta или к банковским сайтам.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// SiteResult — итог проверки адреса через один сервер.
type SiteResult struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	OK      bool    `json:"ok"`
	Latency float64 `json:"latency_ms"`
	Error   string  `json:"error,omitempty"`
}

// NormalizeSiteURL приводит введённое пользователем к пригодному адресу:
// в поле обычно пишут просто «instagram.com».
func NormalizeSiteURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("адрес не указан")
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("адрес непонятен: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("проверять можно только http и https")
	}
	host := u.Hostname()
	if host == "" || !strings.Contains(host, ".") {
		return "", fmt.Errorf("укажите адрес сайта, например instagram.com")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// SiteCheck прогоняет адрес через каждый сервер и сообщает, открывается ли
// он. Результаты отдаются по мере готовности: проверка десятков серверов
// занимает минуты.
func SiteCheck(ctx context.Context, servers []subscription.Server, opt Options,
	onResult func(SiteResult)) []SiteResult {

	results := make([]SiteResult, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, latencyWorkers)

	for i, s := range servers {
		wg.Add(1)
		go func(i int, s subscription.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := SiteResult{Key: s.Key(), Name: s.Name}
			latency, err := TunnelCheck(ctx, s, opt)
			if err != nil {
				r.Error = shortSiteError(err)
			} else {
				r.OK = true
				r.Latency = float64(latency.Microseconds()) / 1000
			}
			results[i] = r
			if onResult != nil {
				onResult(r)
			}
		}(i, s)
	}
	wg.Wait()
	return results
}

// shortSiteError убирает из ошибки служебные подробности: в таблице важно
// лишь то, открылся адрес или нет.
func shortSiteError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset"):
		return "соединение сброшено"
	case strings.Contains(msg, "refused"):
		return "соединение отклонено"
	case strings.Contains(msg, "certificate"):
		return "сервер отверг соединение"
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "EOF"),
		// Обёртка TunnelCheck: внутри может оказаться что угодно, но для
		// таблицы это одно и то же — адрес не открылся.
		strings.Contains(msg, "нет доступа в интернет"),
		strings.Contains(msg, "измерить не удалось"):
		return "нет ответа"
	case strings.Contains(msg, "ответила кодом"):
		return msg
	}
	if len(msg) > 60 {
		return msg[:60] + "…"
	}
	return msg
}

// siteCheckTimeout ограничивает всю проверку: десятки серверов по очереди
// укладываются в минуты, дольше — значит что-то зависло.
const siteCheckTimeout = 15 * time.Minute

// SiteCheckContext создаёт контекст с общим ограничением времени.
func SiteCheckContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, siteCheckTimeout)
}
