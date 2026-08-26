// Package kvas — тонкая обёртка над CLI Кваса и его конфигурационными файлами.
//
// Команды запускаются напрямую через exec без промежуточного шелла, поэтому
// доменные имена и IP из веб-формы не могут превратиться в команду.
package kvas

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// maxOutput ограничивает объём вывода одной команды. Интерактивные ветки
// CLI Кваса на пустом вводе не завершаются, а бесконечно перерисовывают
// меню: без ограничения такая команда съедала бы память роутера.
const maxOutput = 256 << 10

// ErrNeedsConsole означает, что команда ждёт ответа человека и через
// веб-интерфейс выполнена быть не может.
var ErrNeedsConsole = errors.New("команда ожидает ответа в консоли")

// Client выполняет команды kvas.
type Client struct {
	Bin     string
	Timeout time.Duration
}

// limitedWriter копит вывод до предела и сообщает, когда предел пройден.
type limitedWriter struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
	onExceed func()
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if free := w.limit - w.buf.Len(); free > 0 {
		if len(p) < free {
			free = len(p)
		}
		w.buf.Write(p[:free])
	}
	if !w.exceeded && w.buf.Len() >= w.limit {
		w.exceeded = true
		if w.onExceed != nil {
			w.onExceed()
		}
	}
	// Возвращаем полную длину: команда не должна падать по ошибке записи.
	return len(p), nil
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
	// Пустой stdin: на нём интерактивные ветки CLI сразу получают конец
	// ввода. Некоторые из них при этом зацикливаются, поэтому ниже стоит
	// ограничение на объём вывода.
	cmd.Stdin = strings.NewReader("")
	out := &limitedWriter{limit: maxOutput, onExceed: cancel}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(cmd.Environ(), "TERM=dumb", "NO_COLOR=1")

	err := cmd.Run()
	text := StripANSI(out.buf.String())
	switch {
	case out.exceeded:
		return text, fmt.Errorf("kvas %s: %w", strings.Join(args, " "), ErrNeedsConsole)
	case ctx.Err() == context.DeadlineExceeded:
		return text, fmt.Errorf("команда kvas %s не завершилась за %s", strings.Join(args, " "), c.Timeout)
	case err != nil:
		return text, fmt.Errorf("kvas %s: %w", strings.Join(args, " "), err)
	}
	return text, nil
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

	// Тот же расчёт, что и в Run: зацикленное меню CLI выдаёт строки
	// бесконечно, и ждать таймаута в такой ситуации незачем.
	const maxLines = 20000

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 16*1024), 256*1024)
	lines := 0
	looped := false
	for sc.Scan() {
		if line := StripANSI(sc.Text()); line != "" {
			onLine(line)
		}
		lines++
		if lines > maxLines {
			looped = true
			cancel()
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if looped {
			return fmt.Errorf("kvas %s: %w", strings.Join(args, " "), ErrNeedsConsole)
		}
		return fmt.Errorf("kvas %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
