package autovpn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/config"
	"github.com/clrmsc/kvas-web/web/internal/probe"
)

func TestValidateLiveEvery(t *testing.T) {
	for _, v := range []int{1, 5, 60, 1440} {
		if err := ValidateLiveEvery(v); err != nil {
			t.Errorf("период %d должен приниматься: %v", v, err)
		}
	}
	for _, v := range []int{0, -3, 1441} {
		if err := ValidateLiveEvery(v); err == nil {
			t.Errorf("период %d не должен приниматься", v)
		}
	}
}

func TestLiveIntervalDefaults(t *testing.T) {
	if got := (State{}).LiveInterval(); got != 5*time.Minute {
		t.Errorf("пустое состояние: ждали 5 минут, получили %v", got)
	}
	if got := (State{LiveEvery: 15}).LiveInterval(); got != 15*time.Minute {
		t.Errorf("ждали 15 минут, получили %v", got)
	}
}

// spareServers должен предлагать только живые серверы и никогда — текущий:
// переключаться на него же бессмысленно, а на мёртвый — вредно.
func TestSpareServers(t *testing.T) {
	st := State{
		ActiveKey: "b:443",
		Results: []probe.Result{
			{Key: "a:443", Latency: 10},
			{Key: "b:443", Latency: 11},
			{Key: "c:443", Error: "не отвечает"},
			{Key: "d:443", Latency: 20, TunnelError: "нет доступа"},
			{Key: "e:443", Latency: 30},
		},
	}
	m := &Manager{}
	got := m.spareServers(st, 3)
	if len(got) != 2 {
		t.Fatalf("ждали двух кандидатов, получили %d: %+v", len(got), got)
	}
	if got[0].Key != "a:443" || got[1].Key != "e:443" {
		t.Errorf("порядок кандидатов нарушен: %+v", got)
	}
	if len(m.spareServers(st, 1)) != 1 {
		t.Error("предел числа кандидатов не соблюдён")
	}
}

// liveProbe должен ходить наружу именно через SOCKS Кваса.
func TestLiveProbeUsesWorkingProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	proxy := startSOCKSStub(t)

	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.ProxyPort = proxy
	cfg.TunnelURL = target.URL
	m := &Manager{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	latency, err := m.liveProbe(context.Background())
	if err != nil {
		t.Fatalf("проверка через рабочий прокси не прошла: %v", err)
	}
	if latency <= 0 {
		t.Error("задержка не измерена")
	}

	// Прокси упал — проверка обязана это заметить.
	cfg.ProxyPort = freeLocalPort(t)
	m2 := &Manager{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := m2.liveProbe(context.Background()); err == nil {
		t.Error("проверка не заметила отсутствие прокси")
	}
}

// recordLive пишет файл только при смене состояния: сторож срабатывает
// каждые несколько минут, и постоянная запись зря изнашивает флеш.
func TestRecordLiveSavesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	m := &Manager{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), state: DefaultState()}

	m.recordLive(true, 120*time.Millisecond, "", 0)
	first := mustModTime(t, cfg.SubscriptionFile())
	if !m.State().LiveOK || m.State().LiveMS == 0 {
		t.Fatal("итог проверки не сохранён в памяти")
	}

	time.Sleep(20 * time.Millisecond)
	m.recordLive(true, 130*time.Millisecond, "", 0)
	if second := mustModTime(t, cfg.SubscriptionFile()); !second.Equal(first) {
		t.Error("повторная удачная проверка не должна переписывать файл")
	}

	time.Sleep(20 * time.Millisecond)
	m.recordLive(false, 0, "через туннель нет связи", 0)
	if third := mustModTime(t, cfg.SubscriptionFile()); third.Equal(first) {
		t.Error("смена состояния должна сохраняться")
	}
	if m.State().LiveMS != 0 {
		t.Error("при потере связи задержка должна обнуляться")
	}

	m.recordLive(true, 100*time.Millisecond, "", 1)
	if m.State().LiveRepairs != 1 {
		t.Errorf("счётчик починок: ждали 1, получили %d", m.State().LiveRepairs)
	}
}

// startSOCKSStub поднимает простейший SOCKS5-прокси и возвращает его порт:
// он изображает рабочий xray, через который Квас выпускает трафик.
func startSOCKSStub(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSOCKS(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveSOCKS(client net.Conn) {
	defer client.Close()

	// Приветствие: версия, число методов, сами методы.
	head := make([]byte, 2)
	if _, err := io.ReadFull(client, head); err != nil {
		return
	}
	if _, err := io.ReadFull(client, make([]byte, int(head[1]))); err != nil {
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Запрос: версия, команда, резерв, тип адреса.
	req := make([]byte, 4)
	if _, err := io.ReadFull(client, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(client, l); err != nil {
			return
		}
		buf := make([]byte, int(l[0]))
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		host = string(buf)
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(client, portBuf); err != nil {
		return
	}
	port := int(portBuf[0])<<8 | int(portBuf[1])

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		client.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	go io.Copy(upstream, client)
	io.Copy(client, upstream)
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func mustModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл состояния не создан: %v", err)
	}
	return fi.ModTime()
}

// Сценарий целиком: связь через туннель пропала, сторож обязан вернуть её
// сам. Роутер изображают заглушки: «xray» — SOCKS-стаб, который рвёт
// соединения, пока лежит файл-признак поломки, а init-скрипт убирает этот
// файл ровно тогда, когда в конфигурацию попадает исправный сервер.
func TestLiveVerifyRepairsTunnel(t *testing.T) {
	env := newRepairEnv(t, "good.example")

	// Туннель «сломан»: порт слушается, а трафик не ходит.
	env.breakTunnel()
	if _, err := env.m.liveProbe(context.Background()); err == nil {
		t.Fatal("сломанный туннель прошёл проверку связи")
	}

	env.m.liveVerify(context.Background(), env.m.State())

	st := env.m.State()
	if !st.LiveOK {
		t.Fatalf("связь не восстановлена: %s", st.LiveError)
	}
	if st.LiveRepairs != 1 {
		t.Errorf("починок насчитано %d, ожидалась одна", st.LiveRepairs)
	}
	if st.ActiveKey != "good.example:443" {
		t.Errorf("сервер сменился без нужды: %s", st.ActiveKey)
	}
}

// Если текущий сервер не оживает, сторож переходит на следующий из
// рейтинга — но только когда пользователь разрешил менять сервер сам.
func TestLiveVerifySwitchesToSpare(t *testing.T) {
	// Лечит только good.example: перезапуск текущего сервера не поможет.
	env := newRepairEnv(t, "good.example")
	env.setActive("bad.example:443", "плохой")
	env.breakTunnel()

	env.m.liveVerify(context.Background(), env.m.State())

	st := env.m.State()
	if !st.LiveOK {
		t.Fatalf("связь не восстановлена: %s", st.LiveError)
	}
	if st.ActiveKey != "good.example:443" {
		t.Errorf("ожидали переход на запасной сервер, получили %s", st.ActiveKey)
	}
}

func TestLiveVerifyKeepsServerWhenAutoApplyOff(t *testing.T) {
	env := newRepairEnv(t, "good.example")
	env.setActive("bad.example:443", "плохой")
	env.setAutoApply(false)
	env.breakTunnel()

	env.m.liveVerify(context.Background(), env.m.State())

	st := env.m.State()
	if st.LiveOK {
		t.Error("связи нет, а сторож отчитался об успехе")
	}
	if st.ActiveKey != "bad.example:443" {
		t.Errorf("сервер сменился при выключенном автопереключении: %s", st.ActiveKey)
	}
	if !strings.Contains(st.LiveError, "автопереключение выключено") {
		t.Errorf("причина описана невнятно: %q", st.LiveError)
	}
}

// Пока у роутера нет интернета вовсе, трогать туннель нельзя: перебор
// серверов при обрыве у провайдера только рвёт рабочие соединения.
func TestLiveVerifySkipsRepairWithoutInternet(t *testing.T) {
	env := newRepairEnv(t, "good.example")
	env.m.internetUp = func(context.Context) bool { return false }
	env.breakTunnel()

	env.m.liveVerify(context.Background(), env.m.State())

	st := env.m.State()
	if st.LiveOK || st.LiveRepairs != 0 {
		t.Errorf("сторож полез чинить туннель без интернета: %+v", st)
	}
	if !env.stillBroken() {
		t.Error("конфигурация тронута, хотя чинить было нечего")
	}
}

// --- окружение ------------------------------------------------------

type repairEnv struct {
	m       *Manager
	dir     string
	brokenF string
}

// newRepairEnv собирает менеджер с подпиской из двух серверов: healthy —
// тот, с которым SOCKS-стаб начинает работать, второй всегда мёртв.
func newRepairEnv(t *testing.T, healthy string) *repairEnv {
	t.Helper()
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	port := startSwitchableSOCKS(t, broken)

	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w,
			"vless://11111111-2222-3333-4444-555555555555@good.example:443?type=tcp&security=reality&pbk=KEY&sni=example.net&sid=aa&fp=chrome#хороший\n"+
				"vless://11111111-2222-3333-4444-555555555555@bad.example:443?type=tcp&security=reality&pbk=KEY&sni=example.net&sid=aa&fp=chrome#плохой\n")
	}))
	t.Cleanup(sub.Close)

	// «xray» проверяет конфигурацию молча, а init-скрипт решает судьбу
	// туннеля по тому, чей адрес оказался в конфигурации.
	xrayBin := filepath.Join(dir, "xray")
	writeScript(t, xrayBin, "#!/bin/sh\nexit 0\n")
	xrayInit := filepath.Join(dir, "S24xray")
	writeScript(t, xrayInit, fmt.Sprintf(`#!/bin/sh
if grep -q '"address": "%s"' %s/xray.json; then
	rm -f %s
else
	touch %s
fi
exit 0
`, healthy, dir, broken, broken))

	cfg := config.Default()
	cfg.StateDir = dir
	cfg.KvasConf = filepath.Join(dir, "kvas.conf") // без INFACE_CLI: интерфейс не трогаем
	cfg.XrayBin = xrayBin
	cfg.XrayInit = xrayInit
	cfg.XrayConf = filepath.Join(dir, "xray.json")
	cfg.ProxyPort = port
	cfg.TunnelURL = target.URL
	if err := os.WriteFile(cfg.KvasConf, []byte("APP_VERSION=1.1.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		cfg:        cfg,
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:      DefaultState(),
		internetUp: func(context.Context) bool { return true },
	}
	m.state.URL = sub.URL
	m.state.ActiveKey = "good.example:443"
	m.state.ActiveName = "хороший"
	m.state.Results = []probe.Result{
		{Key: "good.example:443", Name: "хороший", Address: "good.example", Port: 443, Latency: 10},
		{Key: "bad.example:443", Name: "плохой", Address: "bad.example", Port: 443, Latency: 12},
	}
	return &repairEnv{m: m, dir: dir, brokenF: broken}
}

func (e *repairEnv) breakTunnel() {
	if err := os.WriteFile(e.brokenF, []byte("1"), 0o644); err != nil {
		panic(err)
	}
}

func (e *repairEnv) stillBroken() bool {
	_, err := os.Stat(e.brokenF)
	return err == nil
}

func (e *repairEnv) setActive(key, name string) {
	e.m.state.ActiveKey = key
	e.m.state.ActiveName = name
}

func (e *repairEnv) setAutoApply(v bool) { e.m.state.AutoApply = v }

// startSwitchableSOCKS поднимает SOCKS5-прокси, который отказывает в
// обслуживании, пока существует указанный файл.
func startSwitchableSOCKS(t *testing.T, brokenFile string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if _, err := os.Stat(brokenFile); err == nil {
				conn.Close()
				continue
			}
			go serveSOCKS(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
