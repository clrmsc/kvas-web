// Package keenetic — минимальный клиент локального RCI-интерфейса роутера.
//
// Роутер отдаёт JSON на 127.0.0.1:79 без аутентификации для запросов
// с самого устройства. Используется только для чтения справочной
// информации: имён устройств из DHCP и списка интерфейсов.
package keenetic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (c *Client) get(ctx context.Context, path string, dst any) error {
	url := "http://" + c.Addr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
