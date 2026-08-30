package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telegramCIDR — официальный список подсетей Telegram.
const telegramCIDR = "https://core.telegram.org/resources/cidr.txt"

func (s *Server) handleNetworksList(w http.ResponseWriter, r *http.Request) {
	items, err := s.networks.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "networks": items})
}

func (s *Server) handleNetworkAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Networks string `json:"networks"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	items := splitLines(body.Networks)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "не указано ни одной подсети")
		return
	}
	added, err := s.networks.Add(items)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("добавлены подсети", "новых", added)
	writeOK(w, fmt.Sprintf("добавлено подсетей: %d", added))
}

func (s *Server) handleNetworkDel(w http.ResponseWriter, r *http.Request) {
	raw, err := url.PathUnescape(r.PathValue("net"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректный адрес запроса")
		return
	}
	if err := s.networks.Remove(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("подсеть удалена", "подсеть", raw)
	writeOK(w, "удалено: "+raw)
}

// handleNetworksTelegram подтягивает официальный список подсетей Telegram.
// Клиент Telegram ходит к дата-центрам по зашитым адресам, не спрашивая
// DNS, поэтому через обычный список доменов его трафик в туннель не попадает.
func (s *Server) handleNetworksTelegram(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchThroughTunnel(r.Context(), telegramCIDR)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// В списке есть и IPv6, но таблица Кваса — только для IPv4.
	var v4 []string
	for _, line := range splitLines(string(body)) {
		if !strings.Contains(line, ":") {
			v4 = append(v4, line)
		}
	}
	if len(v4) == 0 {
		writeError(w, http.StatusBadGateway, "список подсетей пуст")
		return
	}

	added, err := s.networks.Add(v4)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("добавлены подсети Telegram", "всего", len(v4), "новых", added)
	writeOK(w, fmt.Sprintf("подсетей Telegram: %d, из них новых: %d", len(v4), added))
}

// discordCIDR — блок, зарегистрированный на Discord Inc. (RIPE:
// US-DISCORD1). В нём живут голосовые серверы: клиент шлёт на них UDP,
// получив адрес от API, поэтому список доменов такой трафик не ловит.
//
// Списка подсетей по ссылке у Discord нет — сам сервис называет этот
// диапазон в требованиях к сетевым экранам.
var discordCIDR = []string{"66.22.192.0/18"}

// handleNetworksDiscord добавляет подсети голосовых серверов Discord.
func (s *Server) handleNetworksDiscord(w http.ResponseWriter, r *http.Request) {
	added, err := s.networks.Add(discordCIDR)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("добавлены подсети Discord", "всего", len(discordCIDR), "новых", added)
	if added == 0 {
		writeOK(w, "подсети Discord уже добавлены")
		return
	}
	writeOK(w, fmt.Sprintf("подсетей Discord: %d, из них новых: %d", len(discordCIDR), added))
}

// fetchThroughTunnel скачивает страницу, при неудаче повторяя запрос через
// туннель: часть нужных адресов из России напрямую недоступна — например,
// сам список подсетей Telegram.
func (s *Server) fetchThroughTunnel(ctx context.Context, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	direct, err := fetch(ctx, target, "")
	if err == nil {
		return direct, nil
	}
	s.log.Debug("прямой запрос не прошёл, пробуем через туннель", "адрес", target, "err", err)

	proxy := fmt.Sprintf("socks5://127.0.0.1:%d", s.cfg.ProxyPort)
	throughTunnel, tunnelErr := fetch(ctx, target, proxy)
	if tunnelErr != nil {
		return nil, fmt.Errorf("не удалось получить список ни напрямую (%v), ни через туннель (%v)", err, tunnelErr)
	}
	return throughTunnel, nil
}

func fetch(ctx context.Context, target, proxy string) ([]byte, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kvas-web")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("код %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// splitLines разбивает текст на непустые строки без комментариев.
func splitLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}
