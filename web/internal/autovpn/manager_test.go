package autovpn

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/config"
)

// newTestManager поднимает менеджер с подпиской из локального сервера.
// В подписке два адреса: один заведомо закрытый, второй — живой слушатель,
// так что проверка отработает без выхода в интернет.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()

	alive, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { alive.Close() })
	go func() {
		for {
			conn, err := alive.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	alivePort := alive.Addr().(*net.TCPAddr).Port

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	dead.Close()

	body := fmt.Sprintf(
		"vless://11111111-2222-3333-4444-555555555555@127.0.0.1:%d?type=tcp&security=reality&pbk=KEY&sni=example.net&sid=aa&fp=chrome#живой\n"+
			"vless://11111111-2222-3333-4444-555555555555@127.0.0.1:%d?type=tcp&security=reality&pbk=KEY&sni=example.net&sid=aa&fp=chrome#мёртвый\n",
		alivePort, deadPort)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.XrayBin = filepath.Join(dir, "нет-xray")
	cfg.XrayConf = filepath.Join(dir, "xray.json")
	cfg.XrayInit = filepath.Join(dir, "нет-init")

	m, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/subscription/vless/token"
	autoApply := false
	if _, err := m.UpdateSettings(Settings{URL: &url, AutoApply: &autoApply}); err != nil {
		t.Fatal(err)
	}
	return m, url
}

func TestCheckSortsAliveFirst(t *testing.T) {
	m, _ := newTestManager(t)

	run, err := m.StartCheck()
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events {
	}

	st := m.State()
	if len(st.Results) != 2 {
		t.Fatalf("получено %d результатов, ожидалось 2", len(st.Results))
	}
	if !st.Results[0].Alive() {
		t.Errorf("первым должен идти доступный сервер: %+v", st.Results[0])
	}
	if st.Results[1].Alive() {
		t.Errorf("вторым должен идти недоступный сервер: %+v", st.Results[1])
	}
	if st.LastCheck.IsZero() {
		t.Error("время проверки не сохранено")
	}
}

func TestCheckRefusesSecondRun(t *testing.T) {
	m, _ := newTestManager(t)

	run, err := m.StartCheck()
	if err != nil {
		t.Fatal(err)
	}
	// Пока первая проверка не завершилась, вторую запускать нельзя.
	if _, err := m.StartCheck(); err == nil {
		t.Error("параллельный запуск должен отклоняться")
	}
	for range run.Events {
	}

	// После завершения — снова можно.
	run2, err := m.StartCheck()
	if err != nil {
		t.Fatalf("после завершения проверка должна запускаться: %v", err)
	}
	for range run2.Events {
	}
}

func TestCheckSurvivesUnsubscribedListener(t *testing.T) {
	m, _ := newTestManager(t)

	// Никто не читает события: подписчик «закрыл вкладку» сразу.
	run, err := m.StartCheck()
	if err != nil {
		t.Fatal(err)
	}
	_ = run

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !m.Running() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if m.Running() {
		t.Fatal("проверка зависла без читателя событий")
	}
	if len(m.State().Results) != 2 {
		t.Error("результаты должны сохраниться даже без подписчика")
	}
}

func TestUpdateSettingsValidates(t *testing.T) {
	m, _ := newTestManager(t)

	bad := "ftp://пример"
	if _, err := m.UpdateSettings(Settings{URL: &bad}); err == nil {
		t.Error("ссылка не по http должна отвергаться")
	}
	badTime := "25:00"
	if _, err := m.UpdateSettings(Settings{CheckTime: &badTime}); err == nil {
		t.Error("некорректное время должно отвергаться")
	}
	tooMany := 50
	if _, err := m.UpdateSettings(Settings{SpeedTopN: &tooMany}); err == nil {
		t.Error("слишком большое число серверов должно отвергаться")
	}
}

func TestChangingURLResetsResults(t *testing.T) {
	m, url := newTestManager(t)

	run, err := m.StartCheck()
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events {
	}
	if len(m.State().Results) == 0 {
		t.Fatal("результаты не сохранились")
	}

	other := url + "-другая"
	if _, err := m.UpdateSettings(Settings{URL: &other}); err != nil {
		t.Fatal(err)
	}
	if len(m.State().Results) != 0 {
		t.Error("результаты прежней подписки должны сбрасываться при смене ссылки")
	}
}

func TestApplyWithoutXrayFails(t *testing.T) {
	m, _ := newTestManager(t)

	servers, err := m.Servers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = m.Apply(t.Context(), servers[0].Key())
	if err == nil {
		t.Fatal("без xray переключение должно возвращать ошибку")
	}
	if m.State().ActiveKey != "" {
		t.Error("при неудаче активный сервер не должен запоминаться")
	}
}
