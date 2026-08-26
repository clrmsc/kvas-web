// Package keenetic — минимальный клиент локального RCI-интерфейса роутера.
//
// Роутер отдаёт JSON на 127.0.0.1:79 без аутентификации для запросов
// с самого устройства. Используется только для чтения справочной
// информации: имён устройств из DHCP и списка интерфейсов.
package keenetic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client обращается к RCI роутера.
type Client struct {
	Addr string
	HTTP *http.Client
}

// New создаёт клиент с коротким таймаутом: интерфейс локальный,
// и подвисать на нём при отрисовке страницы нельзя.
func New(addr string) *Client {
	return &Client{
		Addr: addr,
		HTTP: &http.Client{Timeout: 3 * time.Second},
	}
}

// Lease — запись DHCP-аренды.
type Lease struct {
	IP   string `json:"ip"`
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

// InterfaceState — состояние сетевого интерфейса роутера.
type InterfaceState struct {
	ID        string `json:"id"`
	Link      string `json:"link"`
	State     string `json:"state"`
	Connected string `json:"connected"`
}

// Up сообщает, что интерфейс поднят и подключён.
func (s InterfaceState) Up() bool {
	return s.Link == "up" && s.Connected == "yes"
}

// Interface возвращает состояние интерфейса, например Proxy21.
func (c *Client) Interface(ctx context.Context, name string) (InterfaceState, error) {
	var st InterfaceState
	err := c.get(ctx, "/rci/show/interface?name="+url.QueryEscape(name), &st)
	return st, err
}

// InterfaceUp поднимает интерфейс. Нужно после перезапуска xray: прокси-
// интерфейс Keenetic теряет соединение с локальным SOCKS и сам не встаёт,
// а маршрут в туннель без него не появляется — трафик уходит в blackhole.
func (c *Client) InterfaceUp(ctx context.Context, name string) error {
	payload := []map[string]any{
		{"interface": map[string]any{"name": name, "up": true}},
	}
	return c.post(ctx, "/rci/", payload)
}

// WaitInterfaceUp ждёт, пока интерфейс сообщит о подключении.
func (c *Client) WaitInterfaceUp(ctx context.Context, name string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var last InterfaceState
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := c.Interface(ctx, name)
		if err == nil {
			last = st
			if st.Up() {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("интерфейс %s не поднялся (link=%s, connected=%s)",
		name, last.Link, last.Connected)
}

// DHCPLeases возвращает известные роутеру устройства с их именами.
func (c *Client) DHCPLeases(ctx context.Context) ([]Lease, error) {
	var payload struct {
		Lease []Lease `json:"lease"`
	}
	if err := c.get(ctx, "/rci/show/ip/dhcp/bindings", &payload); err != nil {
		return nil, err
	}
	return payload.Lease, nil
}

// post отправляет команду RCI. Ответ роутера разбирать не нужно: результат
// проверяется отдельным запросом состояния.
func (c *Client) post(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.Addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("RCI %s: код %d", path, resp.StatusCode)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.Addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("RCI %s: код %d", path, resp.StatusCode)
	}
	// Ответы RCI небольшие, но ограничение защищает от неожиданностей.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}
