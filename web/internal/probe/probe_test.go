package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBetterPrefersSpeedThenLatency(t *testing.T) {
	fast := Result{Latency: 120, Speed: 90}
	slow := Result{Latency: 20, Speed: 30}
	if !Better(fast, slow) {
		t.Error("заметно более быстрый сервер должен выигрывать несмотря на задержку")
	}

	// Скорости близки (в пределах 15%) — решает задержка.
	nearA := Result{Latency: 30, Speed: 92}
	nearB := Result{Latency: 90, Speed: 100}
	if !Better(nearA, nearB) {
		t.Error("при близкой скорости должен выигрывать более отзывчивый сервер")
	}

	// Недоступный сервер всегда хуже доступного.
	dead := Result{Error: "нет связи"}
	if Better(dead, slow) || !Better(slow, dead) {
		t.Error("недоступный сервер не может быть лучшим")
	}

	// Если скорость не мерили, сравниваем по задержке.
	onlyPingA := Result{Latency: 25}
	onlyPingB := Result{Latency: 80}
	if !Better(onlyPingA, onlyPingB) {
		t.Error("без замера скорости решает задержка")
	}
}

func TestSortPutsBestFirst(t *testing.T) {
	results := []Result{
		{Name: "медленный", Latency: 10, Speed: 5},
		{Name: "мёртвый", Error: "нет связи"},
		{Name: "быстрый", Latency: 60, Speed: 80},
	}
	Sort(results)
	if results[0].Name != "быстрый" {
		t.Errorf("первым должен быть быстрый, получили %s", results[0].Name)
	}
	if results[2].Name != "мёртвый" {
		t.Errorf("недоступный должен быть последним, получили %s", results[2].Name)
	}
}

func TestLatencyMeasuresLocalListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	d, err := Latency(context.Background(), ln.Addr().String(), 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 || d > time.Second {
		t.Errorf("неправдоподобная задержка: %s", d)
	}
}

func TestLatencyReportsUnreachable(t *testing.T) {
	// Порт, который заведомо никто не слушает.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if _, err := Latency(context.Background(), addr, 1, 300*time.Millisecond); err == nil {
		t.Error("для закрытого порта ожидалась ошибка")
	}
}

// TestDownloadThroughSocks поднимает игрушечный SOCKS5-прокси и проверяет,
// что клиент проходит рукопожатие и считает скорость.
func TestDownloadThroughSocks(t *testing.T) {
	payload := make([]byte, 512*1024)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer origin.Close()

	proxy := startTestSocks(t)

	opt := DefaultOptions()
	opt.SpeedTestURL = origin.URL
	opt.SpeedTimeout = 5 * time.Second
	opt.SpeedLimit = int64(len(payload))

	mbps, err := download(context.Background(), proxy, opt)
	if err != nil {
		t.Fatal(err)
	}
	if mbps <= 0 {
		t.Errorf("скорость посчитана как %v", mbps)
	}
}

// startTestSocks — минимальный SOCKS5-прокси без аутентификации.
func startTestSocks(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSocksConn(client)
		}
	}()
	return ln.Addr().String()
}

func serveSocksConn(client net.Conn) {
	defer client.Close()

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(client, greeting); err != nil {
		return
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(client, head); err != nil {
		return
	}
	var host string
	switch head[3] {
	case 1:
		buf := make([]byte, 4)
		io.ReadFull(client, buf)
		host = net.IP(buf).String()
	case 3:
		lenBuf := make([]byte, 1)
		io.ReadFull(client, lenBuf)
		buf := make([]byte, lenBuf[0])
		io.ReadFull(client, buf)
		host = string(buf)
	default:
		return
	}
	portBuf := make([]byte, 2)
	io.ReadFull(client, portBuf)
	port := int(portBuf[0])<<8 | int(portBuf[1])

	target, err := net.Dial("tcp", net.JoinHostPort(host, itoa(port)))
	if err != nil {
		client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()

	client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(target, client)
	io.Copy(client, target)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestSpeedErrorKeepsServerAlive(t *testing.T) {
	// Замер скорости мог сорваться (нет xray, недоступен тестовый файл),
	// но сервер, ответивший на проверку задержки, остаётся пригодным.
	r := Result{Latency: 42, SpeedError: "не удалось запустить xray"}
	if !r.Alive() {
		t.Error("ошибка замера скорости не должна вычёркивать сервер")
	}
	dead := Result{Error: "нет связи"}
	if dead.Alive() {
		t.Error("недоступный сервер не может считаться живым")
	}
	if !Better(r, dead) {
		t.Error("сервер без замера скорости лучше недоступного")
	}
}
