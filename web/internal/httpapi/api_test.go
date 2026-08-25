package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/clrmsc/kvas-web/web/internal/auth"
	"github.com/clrmsc/kvas-web/web/internal/autovpn"
	"github.com/clrmsc/kvas-web/web/internal/config"
)

const testPassword = "тестовый-пароль"

// fakeKvas подменяет CLI: скрипт дописывает полученные аргументы в файл,
// чтобы тест мог проверить, что и с какими параметрами было вызвано.
func fakeKvas(t *testing.T, dir string) (bin, calls string) {
	t.Helper()
	calls = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "kvas")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\necho \"выполнено: $@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func newTestServer(t *testing.T) (*httptest.Server, config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls := fakeKvas(t, dir)

	cfg := config.Config{
		Listen:      "127.0.0.1:0",
		KvasBin:     bin,
		KvasConf:    filepath.Join(dir, "kvas.conf"),
		HostsList:   filepath.Join(dir, "kvas.list"),
		TagsList:    filepath.Join(dir, "tags.list"),
		AdblockList: filepath.Join(dir, "block.list"),
		DnsmasqConf: filepath.Join(dir, "dnsmasq.conf"),
		StateDir:    filepath.Join(dir, "state"),
		LogFile:     filepath.Join(dir, "web.log"),
		RCIAddr:     "127.0.0.1:1", // RCI недоступен — обработчики должны это переживать
	}
	write := func(name, body string) {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg.KvasConf, "APP_VERSION=1.1.9\nINFACE_ENT=Proxy21\nroute_full_ip=192.168.1.5\n")
	write(cfg.HostsList, "youtube.com\nchatgpt.com\n")
	write(cfg.TagsList, "[Видео]\nyoutube.com\nvimeo.com\n")
	write(cfg.AdblockList, "ads.example.com\n")
	write(cfg.DnsmasqConf, "port=9753\n")

	am, err := auth.New(cfg.PassFile(), cfg.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	av, err := autovpn.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(cfg, am, av, log, static).Handler())
	t.Cleanup(srv.Close)
	return srv, cfg, calls
}

// login выполняет первичную настройку пароля и возвращает клиент с сессией.
func login(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	body := strings.NewReader(`{"password":"` + testPassword + `"}`)
	resp, err := client.Post(srv.URL+"/api/auth/setup", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("настройка пароля вернула %d", resp.StatusCode)
	}
	return client
}

func TestProtectedEndpointsRequireAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)

	for _, path := range []string{"/api/status", "/api/hosts", "/api/tags", "/api/routes", "/api/adblock"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s без сессии вернул %d, ожидалось 401", path, resp.StatusCode)
		}
	}
}

func TestSetupOnlyOnce(t *testing.T) {
	srv, _, _ := newTestServer(t)
	login(t, srv)

	// Повторная настройка не должна позволять перезаписать пароль без входа.
	resp, err := http.Post(srv.URL+"/api/auth/setup", "application/json",
		strings.NewReader(`{"password":"чужой-пароль"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("повторная настройка вернула %d, ожидалось 409", resp.StatusCode)
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	client := login(t, srv)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/hosts",
		strings.NewReader(`{"domain":"evil.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://attacker.example")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("межсайтовый запрос вернул %d, ожидалось 403", resp.StatusCode)
	}
}

func TestHostAddValidatesAndCallsCLI(t *testing.T) {
	srv, _, calls := newTestServer(t)
	client := login(t, srv)

	// Попытка инъекции команды должна отсекаться до вызова CLI.
	resp, err := client.Post(srv.URL+"/api/hosts", "application/json",
		strings.NewReader(`{"domain":"example.com; reboot"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("домен с инъекцией вернул %d, ожидалось 400", resp.StatusCode)
	}

	resp, err = client.Post(srv.URL+"/api/hosts", "application/json",
		strings.NewReader(`{"domain":"Example.COM"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("добавление домена вернуло %d", resp.StatusCode)
	}

	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(logged)); got != "add example.com" {
		t.Errorf("CLI вызван как %q, ожидалось \"add example.com\"", got)
	}
}

func TestStatusReportsCounts(t *testing.T) {
	srv, _, _ := newTestServer(t)
	client := login(t, srv)

	resp, err := client.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Hosts != 2 {
		t.Errorf("доменов %d, ожидалось 2", st.Hosts)
	}
	if st.Tags != 1 {
		t.Errorf("заквасок %d, ожидалось 1", st.Tags)
	}
	if st.Mode != "vless" {
		t.Errorf("режим %q, ожидался vless (INFACE_ENT=Proxy21)", st.Mode)
	}
	if st.Adblock {
		t.Error("блокировка рекламы не настроена в dnsmasq.conf, ожидалось false")
	}
}

func TestRouteAddMovesIPBetweenLists(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	client := login(t, srv)

	// Адрес уже лежит в route_full_ip; добавляем его как исключение.
	resp, err := client.Post(srv.URL+"/api/routes", "application/json",
		strings.NewReader(`{"type":"exclude","ip":"192.168.1.5"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("добавление правила вернуло %d: %s", resp.StatusCode, body)
	}

	data, err := os.ReadFile(cfg.KvasConf)
	if err != nil {
		t.Fatal(err)
	}
	conf := string(data)
	if !strings.Contains(conf, "route_excluded_ip=192.168.1.5") {
		t.Errorf("адрес не попал в исключения:\n%s", conf)
	}
	if strings.Contains(conf, "route_full_ip=192.168.1.5") {
		t.Errorf("адрес остался в прежнем списке:\n%s", conf)
	}
}

func TestTagEnableAddsAllDomains(t *testing.T) {
	srv, _, calls := newTestServer(t)
	client := login(t, srv)

	resp, err := client.Post(srv.URL+"/api/tags/"+
		strings.ReplaceAll("Видео", " ", "%20")+"/enable", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("включение закваски вернуло %d: %s", resp.StatusCode, body)
	}

	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(logged)); got != "add youtube.com vimeo.com" {
		t.Errorf("CLI вызван как %q", got)
	}
}

func TestUnknownPathServesSPA(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/какая-то/вкладка")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<html>ok</html>") {
		t.Errorf("неизвестный путь вернул %d: %s", resp.StatusCode, body)
	}
}

// cookieJar — минимальная реализация http.CookieJar: httptest.Server
// работает на 127.0.0.1, и стандартный jar здесь избыточен.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie             { return j.cookies }

func TestSetupRejectedFromInternet(t *testing.T) {
	_, cfg, _ := newTestServer(t)

	am, err := auth.New(cfg.PassFile(), cfg.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	av, err := autovpn.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	h := New(cfg, am, av, log,
		fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}}).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:44321" // адрес из интернета
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("настройка из интернета вернула %d, ожидалось 403", rec.Code)
	}
	if am.HasPassword() {
		t.Error("пароль не должен быть установлен запросом извне")
	}
}

func TestSetupAllowedFromLAN(t *testing.T) {
	_, cfg, _ := newTestServer(t)

	am, err := auth.New(cfg.PassFile(), cfg.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	av, err := autovpn.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	h := New(cfg, am, av, log,
		fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}}).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.20:51000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("настройка из домашней сети вернула %d: %s", rec.Code, rec.Body)
	}
	if !am.HasPassword() {
		t.Error("пароль должен быть установлен")
	}
}
