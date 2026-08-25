// Package probe измеряет качество VPN-серверов: задержку до сервера и
// скорость скачивания через поднятый на нём туннель.
package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// socksDialer подключается к целевому адресу через локальный SOCKS5-прокси.
// Своя реализация вместо библиотеки: нужен только метод CONNECT без
// аутентификации, а лишние зависимости в бинарнике для роутера ни к чему.
type socksDialer struct {
	proxyAddr string
}

func (d socksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("через SOCKS5 поддерживается только tcp, запрошено %q", network)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("прокси недоступен: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := socksHandshake(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	// Дальше поток принадлежит вызывающему коду, срок из контекста снимаем.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksHandshake(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("некорректный порт %q", portStr)
	}
	if len(host) > 255 {
		return fmt.Errorf("слишком длинное имя хоста")
	}

	// Приветствие: версия 5, один метод — без аутентификации.
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 5 || resp[1] != 0 {
		return fmt.Errorf("прокси отказал в подключении без аутентификации")
	}

	// Запрос CONNECT с доменным именем: пусть DNS разрешает выходной узел.
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[1] != 0 {
		return fmt.Errorf("прокси вернул ошибку подключения (код %d)", head[1])
	}
	// Дочитываем адрес привязки, чтобы не оставить его в потоке.
	switch head[3] {
	case 1:
		_, err = io.CopyN(io.Discard, conn, 4+2)
	case 3:
		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(conn, lenBuf); err == nil {
			_, err = io.CopyN(io.Discard, conn, int64(lenBuf[0])+2)
		}
	case 4:
		_, err = io.CopyN(io.Discard, conn, 16+2)
	default:
		err = fmt.Errorf("прокси вернул неизвестный тип адреса %d", head[3])
	}
	return err
}
