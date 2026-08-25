package probe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// Result — итог проверки одного сервера.
type Result struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Port    int     `json:"port"`
	Key     string  `json:"key"`
	Latency float64 `json:"latency_ms"` // 0, если сервер недоступен
	Speed   float64 `json:"speed_mbps"` // 0, если скорость не измерялась
	// Error — причина недоступности сервера.
	Error string `json:"error,omitempty"`
	// SpeedError — почему не удалось измерить скорость. Сервер при этом
	// остаётся пригодным: он ответил на проверку задержки, а замер мог
	// сорваться из-за отсутствия xray или недоступности тестового файла.
	SpeedError string `json:"speed_error,omitempty"`
	Checked    string `json:"checked_at"`
}

// Alive сообщает, откликнулся ли сервер на проверку задержки.
func (r Result) Alive() bool { return r.Error == "" && r.Latency > 0 }

// Options — настройки проверки.
type Options struct {
	XrayBin      string        // путь к xray, обычно /opt/sbin/xray
	SpeedTestURL string        // откуда качать при замере скорости
	LatencyTries int           // сколько раз измерять задержку
	DialTimeout  time.Duration // таймаут TCP-подключения
	SpeedTimeout time.Duration // сколько секунд качать
	SpeedLimit   int64         // сколько байт максимум качать
	SpeedTopN    int           // для скольких лучших по задержке мерить скорость
}

// DefaultOptions подобраны так, чтобы суточная проверка нескольких десятков
// серверов не съедала заметный трафик и не грузила роутер надолго.
func DefaultOptions() Options {
	return Options{
		XrayBin:      "/opt/sbin/xray",
		SpeedTestURL: "https://speed.cloudflare.com/__down?bytes=20000000",
		LatencyTries: 3,
		DialTimeout:  4 * time.Second,
		SpeedTimeout: 6 * time.Second,
		SpeedLimit:   8 << 20,
		SpeedTopN:    5,
	}
}

// Latency измеряет время установления TCP-соединения с сервером.
// Берётся лучшая из нескольких попыток: так меньше влияет случайная
// потеря пакета на мобильном канале.
func Latency(ctx context.Context, endpoint string, tries int, timeout time.Duration) (time.Duration, error) {
	if tries < 1 {
		tries = 1
	}
	best := time.Duration(0)
	var lastErr error
	for i := 0; i < tries; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		start := time.Now()
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		elapsed := time.Since(start)
		conn.Close()
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("сервер не отвечает: %w", lastErr)
	}
	return best, nil
}

// Speed поднимает временный экземпляр xray на свободном порту и качает
// через него тестовый файл. Возвращает скорость в мегабитах в секунду.
func Speed(ctx context.Context, s subscription.Server, opt Options) (float64, error) {
	port, err := freePort()
	if err != nil {
		return 0, err
	}

	cfgOpt := subscription.DefaultXrayOptions()
	cfgOpt.ListenPort = port
	cfgOpt.AccessLog = ""
	cfgOpt.ErrorLog = ""
	cfg, err := subscription.XrayConfig(s, cfgOpt)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp("", "kvasweb-probe-*.json")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(cfg); err != nil {
		tmp.Close()
		return 0, err
	}
	tmp.Close()

	runCtx, cancel := context.WithTimeout(ctx, opt.SpeedTimeout+15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, opt.XrayBin, "run", "-c", tmp.Name())
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	// Убиваем всё дерево процессов: xray не должен пережить проверку.
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("не удалось запустить %s: %w", filepath.Base(opt.XrayBin), err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	proxyAddr := net.JoinHostPort(cfgOpt.ListenIP, fmt.Sprint(port))
	if err := waitForPort(runCtx, proxyAddr, 5*time.Second); err != nil {
		return 0, fmt.Errorf("туннель не поднялся: %w", err)
	}

	return download(runCtx, proxyAddr, opt)
}

// download качает тестовый файл через прокси и считает скорость.
func download(ctx context.Context, proxyAddr string, opt Options) (float64, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         socksDialer{proxyAddr: proxyAddr}.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 8 * time.Second,
		},
	}

	reqCtx, cancel := context.WithTimeout(ctx, opt.SpeedTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opt.SpeedTestURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("не удалось скачать тестовый файл: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("тестовый файл отдан с кодом %d", resp.StatusCode)
	}

	start := time.Now()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, opt.SpeedLimit))
	elapsed := time.Since(start)
	// Обрыв по таймауту — норма: значит, за отведённое время скачали столько.
	if err != nil && n == 0 {
		return 0, fmt.Errorf("скачивание не удалось: %w", err)
	}
	if elapsed <= 0 || n == 0 {
		return 0, fmt.Errorf("скорость измерить не удалось")
	}
	return float64(n) * 8 / elapsed.Seconds() / 1e6, nil
}

// freePort просит систему выдать свободный порт и сразу его освобождает.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForPort ждёт, пока xray начнёт принимать подключения.
func waitForPort(ctx context.Context, addr string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("порт %s так и не открылся", addr)
}

// Sort упорядочивает результаты от лучшего к худшему.
func Sort(results []Result) {
	sort.SliceStable(results, func(i, j int) bool { return Better(results[i], results[j]) })
}

// Better сравнивает два результата. Скорость важнее задержки, но при
// близкой скорости (разница до 15%) выигрывает более отзывчивый сервер:
// для мессенджеров и веб-страниц это заметнее лишних мегабит.
func Better(a, b Result) bool {
	if a.Alive() != b.Alive() {
		return a.Alive()
	}
	if !a.Alive() {
		return false
	}
	switch {
	case a.Speed == 0 && b.Speed == 0:
		return a.Latency < b.Latency
	case a.Speed == 0:
		return false
	case b.Speed == 0:
		return true
	}
	fast, slow := a.Speed, b.Speed
	if slow > fast {
		fast, slow = slow, fast
	}
	if slow >= fast*0.85 {
		return a.Latency < b.Latency
	}
	return a.Speed > b.Speed
}
