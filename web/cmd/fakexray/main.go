// Команда fakexray — заглушка xray для локального стенда.
//
// Принимает те же аргументы, что и настоящий xray (`run -c файл`,
// `run -test -c файл`), читает из конфигурации порт входящего SOCKS
// и поднимает обычный SOCKS5-прокси без всякого шифрования: трафик идёт
// напрямую. Этого достаточно, чтобы проверить замер скорости и
// переключение серверов, не имея под рукой роутера.
//
// В пакет для роутера не попадает: собирается только вручную.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
)

func main() {
	var cfgPath string
	testOnly := false
	for i, a := range os.Args {
		switch a {
		case "-test":
			testOnly = true
		case "-c", "-config":
			if i+1 < len(os.Args) {
				cfgPath = os.Args[i+1]
			}
		}
	}
	if cfgPath == "" {
		log.Fatal("не указан файл конфигурации")
	}

	port, err := readPort(cfgPath)
	if err != nil {
		log.Fatalf("конфигурация не подходит: %v", err)
	}
	if testOnly {
		fmt.Println("Configuration OK.")
		return
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("не удалось занять %s: %v", addr, err)
	}
	log.Printf("заглушка xray слушает SOCKS5 на %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("приём соединения: %v", err)
		}
		go serve(conn)
	}
}

func readPort(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var cfg struct {
		Inbounds []struct {
			Port     json.Number `json:"port"`
			Protocol string      `json:"protocol"`
		} `json:"inbounds"`
		Outbounds []struct {
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, err
	}
	if len(cfg.Inbounds) == 0 || len(cfg.Outbounds) == 0 {
		return 0, fmt.Errorf("в конфигурации нет входов или выходов")
	}
	return strconv.Atoi(cfg.Inbounds[0].Port.String())
}

func serve(client net.Conn) {
	defer client.Close()

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(client, greeting); err != nil {
		return
	}
	if _, err := io.CopyN(io.Discard, client, int64(greeting[1])); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(client, head); err != nil {
		return
	}
	host, err := readHost(client, head[3])
	if err != nil {
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(client, portBuf); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBuf))))

	upstream, err := net.Dial("tcp", target)
	if err != nil {
		client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()

	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go io.Copy(upstream, client)
	io.Copy(client, upstream)
}

func readHost(conn net.Conn, kind byte) (string, error) {
	switch kind {
	case 1:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	case 3:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", err
		}
		buf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	case 4:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", err
		}
		return net.IP(buf).String(), nil
	}
	return "", fmt.Errorf("неизвестный тип адреса %d", kind)
}
