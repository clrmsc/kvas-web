// Команда kvasweb — веб-интерфейс Кваса: один статический бинарник,
// который отдаёт SPA и управляет Квасом через его CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/auth"
	"github.com/clrmsc/kvas-web/web/internal/config"
	"github.com/clrmsc/kvas-web/web/internal/httpapi"
	"github.com/clrmsc/kvas-web/web/ui"
)

// version подставляется при сборке через -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kvasweb:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	for _, a := range args {
		if a == "-version" || a == "--version" {
			fmt.Println("kvasweb", version)
			return nil
		}
	}

	cfg, err := config.FromFlags(args)
	if err != nil {
		return err
	}

	logger, closeLog, err := newLogger(cfg.LogFile)
	if err != nil {
		return err
	}
	defer closeLog()

	am, err := auth.New(cfg.PassFile(), cfg.SessionFile())
	if err != nil {
		return fmt.Errorf("не удалось подготовить хранилище паролей: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(cfg, am, logger, ui.FS()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Пишущий таймаут не ставим: импорт списков и обновление отдают
		// прогресс потоком и могут идти минутами.
		IdleTimeout: 2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("веб-интерфейс запущен", "listen", cfg.Listen, "version", version,
			"tls", cfg.TLSCert != "")
		if !am.HasPassword() {
			logger.Warn("пароль не задан: первый вход в браузере предложит его создать")
		}
		var err error
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("получен сигнал завершения")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newLogger пишет и в файл, и в стандартный вывод: файл нужен веб-странице
// журнала, вывод — при запуске вручную из консоли.
func newLogger(path string) (*slog.Logger, func(), error) {
	writers := []io.Writer{os.Stdout}
	closeFn := func() {}

	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
			if err == nil {
				writers = append(writers, f)
				closeFn = func() { f.Close() }
			}
		}
	}

	h := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), closeFn, nil
}
