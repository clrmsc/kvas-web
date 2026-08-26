package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

const (
	// tunnelAttempts — сколько раз пробуем выйти наружу через свежий туннель.
	tunnelAttempts = 3
	// tunnelWarmup — пауза между попытками, чтобы xray успел договориться
	// с сервером.
	tunnelWarmup = 1200 * time.Millisecond
)

// ErrProbeUnavailable означает, что проверить туннель нечем: не нашёлся или
// не запустился xray. Это неисправность роутера, а не сервера, — помечать
// серверы нерабочими в такой ситуации нельзя, иначе автовыбор встанет весь.
var ErrProbeUnavailable = errors.New("проверка туннеля недоступна")

// Result — итог проверки одного сервера.
type Result struct {
	Name    string  `json:"name"`
	Address string  `json:"address"`
	Port    int     `json:"port"`
	Key     string  `json:"key"`
	Latency float64 `json:"latency_ms"` // отклик входного узла; 0, если недоступен
	// Tunnel — задержка запроса, прошедшего через туннель до внешнего сайта.
	// Именно она говорит о качестве сервера: у провайдеров с близкими
	// точками входа отклик самого узла одинаков у всех серверов.
	Tunnel float64 `json:"tunnel_ms"`
	Speed  float64 `json:"speed_mbps"` // 0, если скорость не измерялась
	// Error — причина недоступности сервера.
	Error string `json:"error,omitempty"`
	// SpeedError — почему не удалось измерить скорость. Сервер при этом
	// остаётся пригодным: он ответил на проверку задержки, а замер мог
	// сорваться из-за отсутствия xray или недоступности тестового файла.
	SpeedError string `json:"speed_error,omitempty"`
	// SpeedStale означает, что скорость взята из прошлой проверки: за один
	// раз она измеряется лишь у части серверов.
	SpeedStale bool `json:"speed_stale,omitempty"`
	// TunnelError — почему через сервер не удалось выйти в интернет.
	// Открытый порт ещё не означает работающий туннель.
	TunnelError string `json:"tunnel_error,omitempty"`
	Checked     string `json:"checked_at"`
}

// Reachable сообщает, что входной узел сервера откликнулся.
func (r Result) Reachable() bool { return r.Error == "" && r.Latency > 0 }

// Alive сообщает, что через сервер удалось выйти в интернет. Пока проверка
// туннеля не выполнялась, достаточно отклика входного узла.
func (r Result) Alive() bool {
	if !r.Reachable() {
		return false
	}
	if r.TunnelError != "" {
		return false
	}
	return true
}

// Options — настройки проверки.
type Options struct {
	XrayBin      string        // путь к xray, обычно /opt/sbin/xray
	SpeedTestURL string        // откуда качать при замере скорости
	TunnelURL    string        // что запрашивать при проверке туннеля
	LatencyTries int           // сколько раз измерять задержку
	DialTimeout  time.Duration // таймаут TCP-подключения
	SpeedTimeout time.Duration // сколько секунд качать
	SpeedLimit   int64         // сколько байт максимум качать
	SpeedTopN    int           // для скольких серверов мерить скорость за раз
	// ActiveKey — сервер, который используется сейчас. Его скорость
	// перемеряется всегда: иначе свежие замеры соперников сравнивались бы
	// с его устаревшим значением, и выбор оказывался бы случайным.
	ActiveKey string
}

// DefaultOptions подобраны так, чтобы суточная проверка нескольких десятков
// серверов не съедала заметный трафик и не грузила роутер надолго.
func DefaultOptions() Options {
	return Options{
		XrayBin:      "/opt/sbin/xray",
		SpeedTestURL: "https://speed.cloudflare.com/__down?bytes=20000000",
		// Страница на 204 без тела: замеряет именно задержку, а не канал.
		TunnelURL:    "https://www.gstatic.com/generate_204",
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

// TunnelCheck поднимает туннель до сервера и запрашивает через него
// внешнюю страницу. Возвращает задержку такого запроса — то самое время,
// которое почувствует пользователь, открывая сайт через этот сервер.
//
// Заодно это единственная надёжная проверка работоспособности: открытый
// порт входного узла ничего не говорит о том, ходит ли трафик наружу.
func TunnelCheck(ctx context.Context, s subscription.Server, opt Options) (time.Duration, error) {
	var latency time.Duration
	err := withTunnel(ctx, s, opt, 30*time.Second, func(proxyAddr string) error {
		client := socksClient(proxyAddr, 8*time.Second)

		// Свежий xray принимает соединения на локальном порту раньше, чем
		// успевает договориться с сервером, и первые попытки обрываются.
		// Поэтому даём ему передышку и пробуем несколько раз.
		var lastErr error
		for attempt := 0; attempt < tunnelAttempts; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(tunnelWarmup):
				}
			}

			reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opt.TunnelURL, nil)
			if err != nil {
				cancel()
				return err
			}
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				lastErr = err
				continue
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancel()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("проверочная страница ответила кодом %d", resp.StatusCode)
			}
			// Берём лучшее измерение: первые попытки включают установку
			// Reality-сессии и завышают задержку.
			elapsed := time.Since(start)
			if latency == 0 || elapsed < latency {
				latency = elapsed
			}
		}
		if latency == 0 && lastErr != nil {
			return fmt.Errorf("через туннель нет доступа в интернет: %w", lastErr)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if latency <= 0 {
		return 0, fmt.Errorf("задержку через туннель измерить не удалось")
	}
	return latency, nil
}

// socksClient — HTTP-клиент, ходящий через локальный SOCKS5 временного xray.
func socksClient(proxyAddr string, tlsTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         socksDialer{proxyAddr: proxyAddr}.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: tlsTimeout,
		},
	}
}

// withTunnel поднимает xray с конфигурацией сервера на свободном порту,
// выполняет fn и гарантированно останавливает процесс.
func withTunnel(ctx context.Context, s subscription.Server, opt Options,
	extra time.Duration, fn func(proxyAddr string) error) error {

	port, err := freePort()
	if err != nil {
		return err
	}

	cfgOpt := subscription.DefaultXrayOptions()
	cfgOpt.ListenPort = port
	cfgOpt.AccessLog = ""
	cfgOpt.ErrorLog = ""
	cfg, err := subscription.XrayConfig(s, cfgOpt)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "kvasweb-probe-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(cfg); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	runCtx, cancel := context.WithTimeout(ctx, extra)
	defer cancel()

	cmd := exec.CommandContext(runCtx, opt.XrayBin, "run", "-c", tmp.Name())
	// Вывод xray сохраняем: без него причина сорвавшегося замера выглядит
	// как безликий обрыв соединения с локальным портом.
	var xrayOut boundedBuffer
	cmd.Stdout = &xrayOut
	cmd.Stderr = &xrayOut
	// Убиваем всё дерево процессов: xray не должен пережить проверку.
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: не удалось запустить %s: %v",
			ErrProbeUnavailable, filepath.Base(opt.XrayBin), err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	proxyAddr := net.JoinHostPort(cfgOpt.ListenIP, fmt.Sprint(port))
	if err := waitForPort(runCtx, proxyAddr, 5*time.Second); err != nil {
		// Локальный порт не открылся — виноват xray на роутере, не сервер.
		return fmt.Errorf("%w: туннель не поднялся: %v%s", ErrProbeUnavailable, err, xrayOut.suffix())
	}

	if err := fn(proxyAddr); err != nil {
		return fmt.Errorf("%w%s", err, xrayOut.suffix())
	}
	return nil
}

// Speed поднимает временный экземпляр xray на свободном порту и качает
// через него тестовый файл. Возвращает скорость в мегабитах в секунду.
func Speed(ctx context.Context, s subscription.Server, opt Options) (float64, error) {
	var mbps float64
	err := withTunnel(ctx, s, opt, opt.SpeedTimeout+15*time.Second, func(proxyAddr string) error {
		// Разовый обрыв на первом соединении — обычное дело: сервер может
		// отбить первую попытку, пока туннель прогревается. Пробуем дважды.
		var lastErr error
		for attempt := 1; attempt <= 2; attempt++ {
			v, err := download(ctx, proxyAddr, opt)
			if err == nil {
				mbps = v
				return nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return lastErr
	})
	if err != nil {
		return 0, err
	}
	return mbps, nil
}

// boundedBuffer собирает начало вывода команды: полный лог xray в сообщении
// об ошибке не нужен, а первые строки обычно и содержат причину.
type boundedBuffer struct {
	mu   sync.Mutex
	data []byte
}

const boundedBufferLimit = 2048

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if free := boundedBufferLimit - len(b.data); free > 0 {
		if len(p) < free {
			free = len(p)
		}
		b.data = append(b.data, p[:free]...)
	}
	return len(p), nil
}

// suffix возвращает вывод xray в виде добавки к сообщению об ошибке.
// Из всего вывода берутся строки, похожие на жалобы: баннер версии и
// сообщения [Info] о принятых соединениях ничего не объясняют.
func (b *boundedBuffer) suffix() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := kvas.StripANSI(string(b.data))
	if text == "" {
		return ""
	}

	var interesting []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "[Info]") {
			continue
		}
		if strings.Contains(line, "[Warning]") || strings.Contains(line, "[Error]") ||
			strings.Contains(line, "failed") || strings.Contains(line, "rejected") ||
			strings.Contains(line, "timeout") {
			interesting = append(interesting, line)
		}
	}
	if len(interesting) == 0 {
		return ""
	}

	// Последние жалобы содержательнее первых: к концу видно, на чём всё встало.
	if len(interesting) > 2 {
		interesting = interesting[len(interesting)-2:]
	}
	joined := strings.Join(strings.Fields(strings.Join(interesting, " ")), " ")
	if len(joined) > 240 {
		joined = joined[len(joined)-240:]
	}
	return " (xray: " + joined + ")"
}

// download качает тестовый файл через прокси и считает скорость.
func download(ctx context.Context, proxyAddr string, opt Options) (float64, error) {
	client := socksClient(proxyAddr, 8*time.Second)

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

// latencySignificance — с какой разницы задержка вообще о чём-то говорит.
// У провайдеров с близкими точками входа все серверы отвечают за 2–4 мс,
// и такие различия — шум измерения, а не свойство сервера.
const latencySignificance = 10 // мс

// latencyOf возвращает задержку, по которой стоит сравнивать серверы:
// через туннель, если она известна, иначе отклик входного узла.
func latencyOf(r Result) float64 {
	if r.Tunnel > 0 {
		return r.Tunnel
	}
	return r.Latency
}

// Better сравнивает два результата. Скорость важнее задержки, но при
// близкой скорости (разница до 15%) выигрывает более отзывчивый сервер —
// если его отзывчивость отличается ощутимо, а не на доли миллисекунды.
func Better(a, b Result) bool {
	if a.Alive() != b.Alive() {
		return a.Alive()
	}
	if !a.Alive() {
		return false
	}
	switch {
	case a.Speed == 0 && b.Speed == 0:
		return latencyOf(a) < latencyOf(b)
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
		la, lb := latencyOf(a), latencyOf(b)
		diff := la - lb
		if diff < 0 {
			diff = -diff
		}
		if diff >= latencySignificance {
			return la < lb
		}
		// Задержки неотличимы — решает скорость, пусть и с малым отрывом.
	}
	return a.Speed > b.Speed
}
