// Package config хранит пути к файлам Кваса и настройки веб-сервиса.
package config

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
)

// Config — все внешние зависимости сервиса, собранные в одном месте,
// чтобы тесты могли подменить пути на временный каталог.
type Config struct {
	Listen       string // адрес прослушивания, например 0.0.0.0:8085
	KvasBin      string // /opt/apps/kvas/bin/kvas
	KvasConf     string // /opt/etc/kvas.conf
	HostsList    string // /opt/etc/kvas.list
	TagsList     string // /opt/apps/kvas/etc/conf/tags.list
	AdblockList  string // /opt/etc/adblock/block.list
	DnsmasqConf  string // /opt/etc/dnsmasq.conf
	FailoverConf string // /opt/etc/kvas.failover.conf
	StateDir     string // /opt/etc/kvas-web — пароль, сессии
	LogFile      string // журнал самого веб-сервиса
	TLSCert      string // опционально
	TLSKey       string // опционально
	RCIAddr      string // Keenetic RCI, обычно 127.0.0.1:79

	// Пустые XrayBin и XrayInit означают «искать самостоятельно»:
	// расположение зависит от того, прошла ли настройка Кваса.
	XrayBin      string
	XrayConf     string // /opt/etc/xray/kvas.json — рабочий конфиг туннеля
	XrayInit     string
	ProxyPort    int    // локальный SOCKS-порт xray, который слушает Квас
	SpeedTestURL string // откуда качать при замере скорости
}

// Default возвращает конфигурацию для штатной установки на роутере.
func Default() Config {
	return Config{
		Listen:       "0.0.0.0:8085",
		KvasBin:      "/opt/apps/kvas/bin/kvas",
		KvasConf:     "/opt/etc/kvas.conf",
		HostsList:    "/opt/etc/kvas.list",
		TagsList:     "/opt/apps/kvas/etc/conf/tags.list",
		AdblockList:  "/opt/etc/adblock/block.list",
		DnsmasqConf:  "/opt/etc/dnsmasq.conf",
		FailoverConf: "/opt/etc/kvas.failover.conf",
		StateDir:     "/opt/etc/kvas-web",
		LogFile:      "/opt/var/log/kvas-web.log",
		RCIAddr:      "127.0.0.1:79",

		XrayBin:      "",
		XrayConf:     "/opt/etc/xray/kvas.json",
		XrayInit:     "",
		ProxyPort:    1097,
		SpeedTestURL: "https://speed.cloudflare.com/__down?bytes=20000000",
	}
}

// FromFlags разбирает аргументы командной строки поверх значений по умолчанию.
// Каждый путь можно переопределить переменной окружения — это нужно для
// разработки на хосте, где никакого /opt нет.
func FromFlags(args []string) (Config, error) {
	c := Default()
	fs := flag.NewFlagSet("kvasweb", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", envOr("KVASWEB_LISTEN", c.Listen), "адрес и порт прослушивания")
	fs.StringVar(&c.KvasBin, "kvas-bin", envOr("KVASWEB_KVAS_BIN", c.KvasBin), "путь к исполняемому файлу kvas")
	fs.StringVar(&c.KvasConf, "kvas-conf", envOr("KVASWEB_KVAS_CONF", c.KvasConf), "путь к kvas.conf")
	fs.StringVar(&c.HostsList, "hosts-list", envOr("KVASWEB_HOSTS_LIST", c.HostsList), "путь к kvas.list")
	fs.StringVar(&c.TagsList, "tags-list", envOr("KVASWEB_TAGS_LIST", c.TagsList), "путь к tags.list")
	fs.StringVar(&c.AdblockList, "adblock-list", envOr("KVASWEB_ADBLOCK_LIST", c.AdblockList), "путь к block.list")
	fs.StringVar(&c.DnsmasqConf, "dnsmasq-conf", envOr("KVASWEB_DNSMASQ_CONF", c.DnsmasqConf), "путь к dnsmasq.conf")
	fs.StringVar(&c.FailoverConf, "failover-conf", envOr("KVASWEB_FAILOVER_CONF", c.FailoverConf), "путь к kvas.failover.conf")
	fs.StringVar(&c.StateDir, "state-dir", envOr("KVASWEB_STATE_DIR", c.StateDir), "каталог состояния (пароль, сессии)")
	fs.StringVar(&c.LogFile, "log-file", envOr("KVASWEB_LOG_FILE", c.LogFile), "файл журнала")
	fs.StringVar(&c.TLSCert, "tls-cert", envOr("KVASWEB_TLS_CERT", ""), "сертификат TLS (включает HTTPS)")
	fs.StringVar(&c.TLSKey, "tls-key", envOr("KVASWEB_TLS_KEY", ""), "ключ TLS")
	fs.StringVar(&c.RCIAddr, "rci", envOr("KVASWEB_RCI", c.RCIAddr), "адрес Keenetic RCI")
	fs.StringVar(&c.XrayBin, "xray-bin", envOr("KVASWEB_XRAY_BIN", c.XrayBin), "путь к xray (по умолчанию ищется автоматически)")
	fs.StringVar(&c.XrayConf, "xray-conf", envOr("KVASWEB_XRAY_CONF", c.XrayConf), "путь к рабочему конфигу xray")
	fs.StringVar(&c.XrayInit, "xray-init", envOr("KVASWEB_XRAY_INIT", c.XrayInit), "init-скрипт xray (по умолчанию ищется автоматически)")
	fs.IntVar(&c.ProxyPort, "proxy-port", envIntOr("KVASWEB_PROXY_PORT", c.ProxyPort), "локальный SOCKS-порт xray")
	fs.StringVar(&c.SpeedTestURL, "speedtest-url", envOr("KVASWEB_SPEEDTEST_URL", c.SpeedTestURL), "адрес файла для замера скорости")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	return c, nil
}

// PassFile — файл с хэшем пароля администратора.
func (c Config) PassFile() string { return filepath.Join(c.StateDir, "password") }

// SubscriptionFile — состояние подписки: ссылка, расписание, результаты
// последней проверки.
func (c Config) SubscriptionFile() string { return filepath.Join(c.StateDir, "subscription.json") }

// SessionFile — файл, куда сохраняются активные сессии, чтобы перезапуск
// сервиса не разлогинивал пользователя.
func (c Config) SessionFile() string { return filepath.Join(c.StateDir, "sessions") }

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
