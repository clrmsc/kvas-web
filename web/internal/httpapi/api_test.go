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
	"github.com/clrmsc/kvas-web/web/internal/networks"
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
	write(cfg.KvasConf, "APP_VERSION=1.1.9\nINFACE_ENT=Proxy21\nroute_full_ip=192.168.1.5\nSETUP_FINISHED=true\n")
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

	nets := networks.New(filepath.Join(dir, "networks.list"))
	srv := httptest.NewServer(New(cfg, am, av, nets, log, static).Handler())
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
	h := New(cfg, am, av, networks.New(filepath.Join(cfg.StateDir, "networks.list")), log,
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
	h := New(cfg, am, av, networks.New(filepath.Join(cfg.StateDir, "networks.list")), log,
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

func TestTagsFileFallsBackToPackagePath(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()

	// Путь из настроек существует — берём его.
	existing := filepath.Join(dir, "tags.list")
	if err := os.WriteFile(existing, []byte("[Видео]\nyoutube.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.TagsList = existing
	s := &Server{cfg: cfg}
	if got := s.tagsFile(); got != existing {
		t.Errorf("получено %q, ожидалось %q", got, existing)
	}

	// Путь не существует и запасных на этой машине нет — возвращается
	// исходный, чтобы сообщение об ошибке указывало на понятный файл.
	cfg.TagsList = filepath.Join(dir, "нет-файла")
	s = &Server{cfg: cfg}
	if got := s.tagsFile(); got != cfg.TagsList && got != "/opt/apps/kvas/etc/conf/tags.list" {
		t.Errorf("неожиданный путь %q", got)
	}
}

func TestOperationsRejectedBeforeSetup(t *testing.T) {
	srv, cfg, calls := newTestServer(t)
	client := login(t, srv)

	// Возвращаем конфигурацию в состояние «kvas setup ещё не выполнялся»:
	// в таком виде CLI Кваса уходит в интерактивный мастер и на пустом
	// вводе зацикливается, поэтому операция должна отклоняться до вызова.
	if err := os.WriteFile(cfg.KvasConf, []byte("APP_VERSION=1.1.9\nSETUP_FINISHED=\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := client.Post(srv.URL+"/api/hosts", "application/json",
		strings.NewReader(`{"domain":"example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("до настройки Кваса ожидался код 409, получено %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "kvas setup") {
		t.Errorf("ответ должен подсказывать, что делать: %s", body)
	}

	// Главное: CLI не вызывался вовсе.
	if data, err := os.ReadFile(calls); err == nil && len(data) > 0 {
		t.Errorf("CLI не должен запускаться до настройки, но был вызван: %s", data)
	}

	// В сводке состояния это видно интерфейсу.
	st, err := client.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Body.Close()
	var status statusResponse
	if err := json.NewDecoder(st.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.SetupFinished {
		t.Error("статус должен сообщать, что настройка не завершена")
	}
}

func TestImportAddsToListInsteadOfReplacing(t *testing.T) {
	srv, cfg, calls := newTestServer(t)
	client := login(t, srv)

	// В списке уже есть домены; импорт не должен их потерять.
	resp, err := client.Post(srv.URL+"/api/hosts/import", "application/json",
		strings.NewReader(`{"domains":"netflix.com\nyoutube.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("импорт вернул %d: %s", resp.StatusCode, body)
	}

	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	cmds := strings.TrimSpace(string(logged))

	// Вызывается add, а не import: последний заменяет список целиком.
	if !strings.Contains(cmds, "add netflix.com") {
		t.Errorf("ожидался вызов add с новым доменом, получено: %q", cmds)
	}
	if strings.Contains(cmds, "import") {
		t.Errorf("import заменяет список и в этом режиме использоваться не должен: %q", cmds)
	}
	// youtube.com уже в списке — повторно добавлять его незачем.
	if strings.Contains(cmds, "youtube.com") {
		t.Errorf("уже имеющийся домен не должен добавляться заново: %q", cmds)
	}

	// Копия прежнего списка сохранена.
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "kvas.list.bak")); err != nil {
		t.Errorf("копия списка не создана: %v", err)
	}
}

func TestImportCanReplaceListExplicitly(t *testing.T) {
	srv, _, calls := newTestServer(t)
	client := login(t, srv)

	resp, err := client.Post(srv.URL+"/api/hosts/import", "application/json",
		strings.NewReader(`{"domains":"netflix.com","replace":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("импорт с заменой вернул %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	logged, _ := os.ReadFile(calls)
	if !strings.Contains(string(logged), "import") {
		t.Errorf("для замены списка ожидался вызов import, получено: %q\nответ: %s", logged, body)
	}
}

func TestXrayStatusRequiresAuthAndReportsVersion(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Без сессии — отказ, как и у остальных операций.
	resp, err := http.Get(srv.URL + "/api/xray")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("без входа ожидался код 401, получено %d", resp.StatusCode)
	}

	client := login(t, srv)
	resp, err = client.Get(srv.URL + "/api/xray")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус xray вернул %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["ok"] != true {
		t.Errorf("неожиданный ответ: %v", payload)
	}
	// Без параметра check на GitHub не ходим — поля о последней версии
	// появляться не должны.
	if _, ok := payload["latest"]; ok {
		t.Error("проверка обновления не должна выполняться без запроса")
	}
}

// Настройки контроля связи должны сохраняться и возвращаться, а негодный
// период — отклоняться: сторож с нулевым интервалом крутился бы вхолостую.
func TestLiveCheckSettingsRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	client := login(t, srv)

	var initial struct {
		LiveCheck bool `json:"live_check"`
		LiveEvery int  `json:"live_every_minutes"`
	}
	getJSON(t, client, srv.URL+"/api/subscription", &initial)
	if !initial.LiveCheck || initial.LiveEvery != 5 {
		t.Errorf("по умолчанию ждали включённую проверку раз в 5 минут, получили %+v", initial)
	}

	resp, err := client.Post(srv.URL+"/api/subscription", "application/json",
		strings.NewReader(`{"live_check":false,"live_every_minutes":30}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("сохранение настроек вернуло %d", resp.StatusCode)
	}

	var saved struct {
		LiveCheck bool   `json:"live_check"`
		LiveEvery int    `json:"live_every_minutes"`
		CheckTime string `json:"check_time"`
	}
	getJSON(t, client, srv.URL+"/api/subscription", &saved)
	if saved.LiveCheck || saved.LiveEvery != 30 {
		t.Errorf("настройки не сохранились: %+v", saved)
	}
	if saved.CheckTime != "04:30" {
		t.Errorf("время суточной проверки затёрлось: %q", saved.CheckTime)
	}

	resp, err = client.Post(srv.URL+"/api/subscription", "application/json",
		strings.NewReader(`{"live_every_minutes":0}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("нулевой период вернул %d, ожидалось 400", resp.StatusCode)
	}
}

func getJSON(t *testing.T, client *http.Client, url string, into any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s вернул %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("ответ %s не разобран: %v", url, err)
	}
}
