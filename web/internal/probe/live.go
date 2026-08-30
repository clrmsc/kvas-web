package probe

// live.go — проверка уже работающего туннеля. В отличие от TunnelCheck
// здесь ничего не поднимается: запрос идёт через рабочий SOCKS Кваса,
// то есть проверяется ровно тот путь, которым ходят устройства в доме.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// liveAttempts — разовый обрыв бывает и на исправном туннеле, поэтому
// приговор выносим только после нескольких неудач подряд.
const liveAttempts = 2

// ProxyCheck запрашивает внешнюю страницу через работающий SOCKS-прокси
// и возвращает задержку такого запроса.
func ProxyCheck(ctx context.Context, proxyAddr, url string, timeout time.Duration) (time.Duration, error) {
	client := socksClient(proxyAddr, timeout)
	return httpProbe(ctx, client, url, timeout, liveAttempts)
}

// InternetUp сообщает, есть ли у роутера интернет вообще. Проверяем
// TCP-соединением с публичными DNS: без DNS-запроса и без TLS, поэтому
// быстро и не зависит от того, что именно заблокировано.
//
// Нужно, чтобы отличить сломанный туннель от пропавшего канала: пока
// у провайдера обрыв, перебирать серверы подписки бессмысленно.
func InternetUp(ctx context.Context, timeout time.Duration) bool {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

// httpProbe выполняет запрос указанным клиентом и возвращает лучшее время
// из нескольких попыток.
func httpProbe(ctx context.Context, client *http.Client, url string,
	timeout time.Duration, attempts int) (time.Duration, error) {

	var best time.Duration
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return 0, err
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
			lastErr = fmt.Errorf("проверочная страница ответила кодом %d", resp.StatusCode)
			continue
		}
		elapsed := time.Since(start)
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	if best == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("ответа нет")
		}
		return 0, lastErr
	}
	return best, nil
}
