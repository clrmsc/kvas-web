package subscription

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Формат повторяет то, что отдают провайдеры: метаданные в комментариях
// и ссылки на серверы. Идентификаторы здесь вымышленные.
const sampleBody = `
#profile-title: base64:VlBORA==
#profile-update-interval: 1
#support-url: https://t.me/example

vless://11111111-2222-3333-4444-555555555555@n02.example.net:1042?type=tcp&security=reality&encryption=none&pbk=abcPUBKEY&fp=firefox&sni=n01.example.net&sid=eeecbb&spx=%2F&flow=xtls-rprx-vision#🇨🇦 Canada - Toronto
vless://11111111-2222-3333-4444-555555555555@n07.example.net:1042?type=tcp&security=reality&encryption=none&pbk=abcPUBKEY&fp=firefox&sni=n01.example.net&sid=eeecbb&spx=%2F&flow=xtls-rprx-vision#🇳🇱 Netherlands - Amsterdam
trojan://ignored@example.com:443#пропускаем
`

func TestParseSubscription(t *testing.T) {
	servers, err := Parse(sampleBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("разобрано %d серверов, ожидалось 2", len(servers))
	}

	first := servers[0]
	if first.Name != "🇨🇦 Canada - Toronto" {
		t.Errorf("имя %q", first.Name)
	}
	if first.Address != "n02.example.net" || first.Port != 1042 {
		t.Errorf("адрес %s", first.Endpoint())
	}
	if first.Security != "reality" || first.PublicKey != "abcPUBKEY" {
		t.Errorf("параметры reality разобраны неверно: %+v", first)
	}
	if first.SNI != "n01.example.net" || first.Flow != "xtls-rprx-vision" {
		t.Errorf("sni/flow разобраны неверно: %+v", first)
	}
}

func TestParseBase64Subscription(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(sampleBody))
	servers, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Errorf("из base64 разобрано %d серверов, ожидалось 2", len(servers))
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	for _, body := range []string{"", "   ", "#только комментарий\n"} {
		if _, err := Parse(body); err == nil {
			t.Errorf("для %q ожидалась ошибка", body)
		}
	}
}

func TestParseVlessRequiresRealityKey(t *testing.T) {
	link := "vless://11111111-2222-3333-4444-555555555555@example.net:443?type=tcp&security=reality#нет ключа"
	if _, err := ParseVless(link); err == nil {
		t.Error("ссылка reality без публичного ключа должна отвергаться")
	}
}

func TestXrayConfigForReality(t *testing.T) {
	servers, err := Parse(sampleBody)
	if err != nil {
		t.Fatal(err)
	}
	opt := DefaultXrayOptions()
	data, err := XrayConfig(servers[0], opt)
	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		Inbounds []struct {
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []struct {
			Settings struct {
				Vnext []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
					Users   []struct {
						ID   string `json:"id"`
						Flow string `json:"flow"`
					} `json:"users"`
				} `json:"vnext"`
			} `json:"settings"`
			StreamSettings struct {
				Network         string `json:"network"`
				Security        string `json:"security"`
				RealitySettings struct {
					PublicKey  string `json:"publicKey"`
					ServerName string `json:"serverName"`
				} `json:"realitySettings"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("конфигурация не является корректным JSON: %v", err)
	}
	if cfg.Inbounds[0].Port != opt.ListenPort || cfg.Inbounds[0].Protocol != "socks" {
		t.Errorf("вход настроен неверно: %+v", cfg.Inbounds[0])
	}
	out := cfg.Outbounds[0]
	if out.Settings.Vnext[0].Address != "n02.example.net" {
		t.Errorf("адрес сервера %q", out.Settings.Vnext[0].Address)
	}
	if out.Settings.Vnext[0].Users[0].Flow != "xtls-rprx-vision" {
		t.Error("для reality должен подставляться flow")
	}
	if out.StreamSettings.RealitySettings.PublicKey != "abcPUBKEY" {
		t.Errorf("публичный ключ не подставлен: %+v", out.StreamSettings)
	}
}

func TestXrayConfigXhttpHasNoFlow(t *testing.T) {
	link := "vless://11111111-2222-3333-4444-555555555555@example.net:443?type=xhttp&security=tls&sni=example.net&path=%2Fdl#xhttp"
	s, err := ParseVless(link)
	if err != nil {
		t.Fatal(err)
	}
	data, err := XrayConfig(s, DefaultXrayOptions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\"flow\"") {
		t.Error("для xhttp flow добавляться не должен — xray отвергнет такой конфиг")
	}
}

func TestMaskURLHidesToken(t *testing.T) {
	raw := "https://example.io/subscription/vless/0123456789abcdef0123456789abcdef"
	masked := MaskURL(raw)
	if strings.Contains(masked, "0123456789abcdef") {
		t.Errorf("токен виден в маске: %s", masked)
	}
	if !strings.HasPrefix(masked, "https://example.io/subscription/vless/") {
		t.Errorf("узнаваемая часть ссылки должна оставаться: %s", masked)
	}
	if !strings.HasSuffix(masked, raw[len(raw)-4:]) {
		t.Errorf("хвост для сверки должен сохраняться: %s", masked)
	}
}

func TestFetchErrorHidesToken(t *testing.T) {
	// Порт заведомо закрыт: получаем сетевую ошибку с подставленной ссылкой.
	secret := "http://127.0.0.1:1/subscription/vless/секретный-токен-подписки"
	_, err := Fetch(t.Context(), secret)
	if err == nil {
		t.Fatal("ожидалась ошибка подключения")
	}
	if strings.Contains(err.Error(), "секретный-токен-подписки") {
		t.Errorf("токен попал в текст ошибки: %v", err)
	}
}
