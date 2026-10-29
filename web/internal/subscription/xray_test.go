package subscription

import (
	"encoding/json"
	"testing"
)

// Конфигурация обязана закреплять IPv4: адрес сервера подписки часто
// резолвится и в IPv6, которого на домашнем роутере нет, а свежие сборки
// xray в этом случае выбирают IPv6 и молча ждут таймаута.
func TestXrayConfigPinsIPv4(t *testing.T) {
	srv := Server{
		Name: "проверка", Address: "example.net", Port: 443,
		ID:       "11111111-2222-3333-4444-555555555555",
		Security: "reality", Network: "tcp",
		PublicKey: "KEY", SNI: "example.com", ShortID: "aa", Fingerprint: "chrome",
	}
	data, err := XrayConfig(srv, DefaultXrayOptions())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			StreamSettings struct {
				Sockopt struct {
					DomainStrategy string `json:"domainStrategy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outbounds) == 0 {
		t.Fatal("в конфигурации нет исходящего соединения")
	}
	if got := cfg.Outbounds[0].StreamSettings.Sockopt.DomainStrategy; got != "UseIPv4" {
		t.Errorf("стратегия выбора адреса: %q, ожидалось UseIPv4", got)
	}
}
