package kvas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeBin создаёт исполняемый скрипт-заглушку.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kvas")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStopsLoopingCommand(t *testing.T) {
	// Так ведёт себя CLI Кваса до первичной настройки: мастер перерисовывает
	// меню, читает пустой ввод и начинает заново.
	bin := fakeBin(t, `while true; do echo "Выберите номер 1-5, H, R, S, или Q-выход:"; done`)
	c := NewClient(bin)
	c.Timeout = 30 * time.Second

	start := time.Now()
	out, err := c.Run(context.Background(), "add", "example.com")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNeedsConsole) {
		t.Fatalf("ожидалась ErrNeedsConsole, получено %v", err)
	}
	if elapsed >= c.Timeout {
		t.Errorf("команда должна прерваться до таймаута, заняла %s", elapsed)
	}
	if len(out) > maxOutput+1024 {
		t.Errorf("вывод не ограничен: %d байт", len(out))
	}
}

func TestRunReturnsOutput(t *testing.T) {
	bin := fakeBin(t, `echo "домен добавлен: $2"`)
	out, err := NewClient(bin).Run(context.Background(), "add", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if out != "домен добавлен: example.com" {
		t.Errorf("получено %q", out)
	}
}

func TestRunStreamStopsLoopingCommand(t *testing.T) {
	bin := fakeBin(t, `while true; do echo "меню"; done`)
	c := NewClient(bin)
	c.Timeout = 30 * time.Second

	lines := 0
	start := time.Now()
	err := c.RunStream(context.Background(), func(string) { lines++ }, "import", "/tmp/список")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNeedsConsole) {
		t.Fatalf("ожидалась ErrNeedsConsole, получено %v", err)
	}
	if elapsed >= c.Timeout {
		t.Errorf("поток должен прерваться до таймаута, занял %s", elapsed)
	}
	if lines == 0 {
		t.Error("строки до прерывания должны были дойти до обработчика")
	}
}

func TestRunReportsFailure(t *testing.T) {
	bin := fakeBin(t, `echo "домен не найден" >&2; exit 1`)
	out, err := NewClient(bin).Run(context.Background(), "del", "example.com")
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if errors.Is(err, ErrNeedsConsole) {
		t.Error("обычный сбой не должен выглядеть как ожидание консоли")
	}
	if out != "домен не найден" {
		t.Errorf("вывод команды должен возвращаться: %q", out)
	}
}
