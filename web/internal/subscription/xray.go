package subscription

import (
	"encoding/json"
	"fmt"
)

// XrayOptions — параметры генерации конфигурации xray.
type XrayOptions struct {
	ListenIP   string // адрес локального SOCKS-входа
	ListenPort int    // порт локального SOCKS-входа
	AccessLog  string
	ErrorLog   string
}

// DefaultXrayOptions повторяет то, что Квас использует для рабочего
// туннеля: SOCKS на 127.0.0.1:1097.
func DefaultXrayOptions() XrayOptions {
	return XrayOptions{
		ListenIP:   "127.0.0.1",
		ListenPort: 1097,
		AccessLog:  "/tmp/log/xray-access.log",
		ErrorLog:   "/tmp/log/xray-errors.log",
	}
}

// XrayConfig собирает конфигурацию xray для одного сервера.
// Формат совпадает с тем, что Квас генерирует в vless_link_parse,
// но собирается через структуры, а не склейкой строк.
func XrayConfig(s Server, opt XrayOptions) ([]byte, error) {
	if s.Address == "" || s.Port == 0 || s.ID == "" {
		return nil, fmt.Errorf("сервер %q описан не полностью", s.Name)
	}

	user := map[string]any{
		"id":         s.ID,
		"encryption": "none",
	}
	// flow применим только к reality поверх обычного транспорта:
	// для xhttp xray отвергает конфигурацию с flow.
	if s.Security == "reality" && s.Network != "xhttp" {
		flow := s.Flow
		if flow == "" {
			flow = "xtls-rprx-vision"
		}
		user["flow"] = flow
	}

	stream := map[string]any{
		"network": s.Network,
	}
	switch s.Security {
	case "reality":
		stream["security"] = "reality"
		stream["realitySettings"] = map[string]any{
			"publicKey":   s.PublicKey,
			"fingerprint": s.Fingerprint,
			"serverName":  s.SNI,
			"shortId":     s.ShortID,
			"spiderX":     s.SpiderX,
		}
	case "tls":
		stream["security"] = "tls"
		stream["tlsSettings"] = map[string]any{
			"serverName":  s.SNI,
			"fingerprint": s.Fingerprint,
		}
	default:
		stream["security"] = "none"
	}

	switch s.Network {
	case "tcp":
		stream["tcpSettings"] = map[string]any{
			"header": map[string]any{"type": "none"},
		}
	case "ws":
		ws := map[string]any{"path": firstNonEmpty(s.Path, "/")}
		if s.HostHeader != "" {
			ws["headers"] = map[string]any{"Host": s.HostHeader}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": s.ServiceName}
	case "xhttp":
		stream["xhttpSettings"] = map[string]any{"path": firstNonEmpty(s.Path, "/")}
	}

	cfg := map[string]any{
		"log": map[string]any{
			"access":   opt.AccessLog,
			"error":    opt.ErrorLog,
			"loglevel": "error",
		},
		"routing": map[string]any{
			"rules":          []any{},
			"domainStrategy": "AsIs",
		},
		"inbounds": []any{
			map[string]any{
				"listen":   opt.ListenIP,
				"port":     opt.ListenPort,
				"protocol": "socks",
				"settings": map[string]any{"udp": true},
			},
		},
		"outbounds": []any{
			map[string]any{
				"tag":      "vless",
				"protocol": "vless",
				"settings": map[string]any{
					"vnext": []any{
						map[string]any{
							"address": s.Address,
							"port":    s.Port,
							"users":   []any{user},
						},
					},
				},
				"streamSettings": stream,
			},
		},
	}
	return json.MarshalIndent(cfg, "", "    ")
}
