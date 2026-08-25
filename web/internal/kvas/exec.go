// Package kvas — тонкая обёртка над CLI Кваса и его конфигурационными файлами.
//
// Команды запускаются напрямую через exec без промежуточного шелла, поэтому
// доменные имена и IP из веб-формы не могут превратиться в команду.
package kvas

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Client выполняет команды kvas.
type Client struct {
	Bin     string
	Timeout time.Duration
}

// NewClient создаёт клиент с разумным таймаутом по умолчанию.
func NewClient(bin string) *Client {
	return &Client{Bin: bin, Timeout: 2 * time.Minute}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI убирает управляющие последовательности — CLI Кваса рисует
// цветные рамки, в JSON они не нужны.
func StripANSI(s string) string {
	return strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
}

// Run выполняет kvas с аргументами и возвращает очищенный вывод.
// Отдельно stdout и stderr не разделяются: CLI пишет сообщения в оба потока.
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	// Пустой stdin, чтобы интерактивные ветки CLI не подвисали в ожидании ответа.
	cmd.Stdin = strings.NewReader("")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Env = append(cmd.Environ(), "TERM=dumb", "NO_COLOR=1")

	err := cmd.Run()
	out := StripANSI(buf.String())
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("команда kvas %s не завершилась за %s", strings.Join(args, " "), c.Timeout)
	}
	if err != nil {
		return out, fmt.Errorf("kvas %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// RunStream выполняет команду и отдаёт её вывод построчно по мере появления.
// Используется для долгих операций (импорт списка, обновление), где важно
// показывать прогресс, а не ждать несколько минут в тишине.
func (c *Client) RunStream(ctx context.Context, onLine func(string), args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(cmd.Environ(), "TERM=dumb", "NO_COLOR=1")

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 16*1024), 256*1024)
	for sc.Scan() {
		if line := StripANSI(sc.Text()); line != "" {
			onLine(line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("kvas %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
