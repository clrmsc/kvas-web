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
	"github.com/clrmsc/kvas-web/web/internal/autovpn"
	"github.com/clrmsc/kvas-web/web/internal/config"
	"github.com/clrmsc/kvas-web/web/internal/httpapi"
	"github.com/clrmsc/kvas-web/web/internal/kvas"
	"github.com/clrmsc/kvas-web/web/internal/networks"
	"github.com/clrmsc/kvas-web/web/internal/selfupd"
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
	checkOnly := false
	updateOnly := false
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-version", "--version":
			fmt.Println("kvasweb", version)
			return nil
		case "-check", "--check":
			// Диагностический режим: проверить серверы подписки и выйти.
			// Нужен, чтобы не дожидаться суточной проверки при отладке.
			checkOnly = true
		case "-update", "--update":
			// Проверить обновление пакета и, если оно есть, поставить.
			updateOnly = true
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered

	cfg, err := config.FromFlags(args)
	if err != nil {
		return err
	}

	// В диагностическом режиме журнал идёт только в файл: иначе его строки
	// перемешиваются с таблицей результатов.
	logger, closeLog, err := newLogger(cfg.LogFile, !checkOnly)
	if err != nil {
		return err
	}
	defer closeLog()

	am, err := auth.New(cfg.PassFile(), cfg.SessionFile())
	if err != nil {
		return fmt.Errorf("не удалось подготовить хранилище паролей: %w", err)
	}

	av, err := autovpn.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("не удалось подготовить подписку: %w", err)
	}

	if checkOnly {
		return runCheck(av)
	}
	if updateOnly {
		return runSelfUpdate(cfg)
	}

	nets := networks.New(cfg.NetworksFile())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpapi.New(cfg, am, av, nets, logger, ui.FS()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Пишущий таймаут не ставим: импорт списков и обновление отдают
		// прогресс потоком и могут идти минутами.
		IdleTimeout: 2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Суточная проверка серверов подписки живёт столько же, сколько сервис.
	go av.RunScheduler(ctx)

	// Туннель поднимаем сам, если он лёг: `kvas upgrade` удаляет его
	// конфигурацию, да и после перезагрузки роутера не всё встаёт.
	go av.WatchTunnel(ctx, 5*time.Minute)

	// Подсети возвращаем в таблицу регулярно: её пересоздаёт `kvas init`,
	// а после перезагрузки роутера она и вовсе пуста.
	go nets.KeepApplied(ctx, 10*time.Minute, func(n int, err error) {
		if err != nil {
			logger.Warn("подсети не применены", "err", err)
			return
		}
		if n > 0 {
			logger.Info("подсети применены", "количество", n)
		}
	})

	// Пути к файлам Кваса различаются между установками, поэтому пишем
	// в журнал то, что сервис нашёл: по этой строке видно, почему,
	// например, список заквасок оказался пустым или не нашёлся xray.
	logger.Info("файлы Кваса",
		"cli", kvas.FindFile(cfg.KvasBin),
		"конфиг", kvas.FindFile(cfg.KvasConf),
		"домены", kvas.FindFile(cfg.HostsList),
		"закваски", kvas.FindFile(append([]string{cfg.TagsList}, kvas.TagsCandidates...)...),
		"xray", kvas.FindFile(append([]string{cfg.XrayBin}, kvas.XrayBinCandidates...)...),
		"xray-init", kvas.FindFile(append([]string{cfg.XrayInit}, kvas.XrayInitCandidates...)...))

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

// newLogger пишет в файл, а при toStdout — ещё и в стандартный вывод:
// файл нужен веб-странице журнала, вывод — при запуске вручную из консоли.
func newLogger(path string, toStdout bool) (*slog.Logger, func(), error) {
	var writers []io.Writer
	if toStdout {
		writers = append(writers, os.Stdout)
	}
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

	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	h := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), closeFn, nil
}

// runCheck выполняет одну проверку серверов подписки и печатает итог.
// Автоприменение при этом работает так же, как по расписанию.
func runCheck(av *autovpn.Manager) error {
	if !av.State().Configured() {
		return fmt.Errorf("подписка не настроена — задайте ссылку в веб-интерфейсе")
	}

	run, err := av.StartCheck()
	if err != nil {
		return err
	}
	for res := range run.Events {
		name := trim(res.Name, 34)
		switch {
		case res.Error != "":
			fmt.Printf("%-34s узел недоступен: %s\n", name, res.Error)
		case res.TunnelError != "":
			fmt.Printf("%-34s туннель не работает: %s\n", name, res.TunnelError)
		case res.Speed > 0:
			stale := ""
			if res.SpeedStale {
				stale = " (прошлый замер)"
			}
			// Пока туннель не проверен, задержки через него ещё нет —
			// показывать её нулём было бы враньём.
			if res.Tunnel > 0 {
				fmt.Printf("%-34s %6.0f мс  %6.1f Мбит/с%s\n", name, res.Tunnel, res.Speed, stale)
			} else {
				fmt.Printf("%-34s %6s     %6.1f Мбит/с%s\n", name, "—", res.Speed, stale)
			}
		case res.SpeedError != "":
			fmt.Printf("%-34s %6.0f мс  замер скорости не удался: %s\n",
				name, res.Tunnel, res.SpeedError)
		case res.Tunnel > 0:
			fmt.Printf("%-34s %6.0f мс через туннель\n", name, res.Tunnel)
		default:
			fmt.Printf("%-34s %6.0f мс отклик узла\n", name, res.Latency)
		}
	}

	st := av.State()
	fmt.Println()
	if st.LastError != "" {
		fmt.Println("ошибка:", st.LastError)
	}
	if st.ActiveName != "" {
		fmt.Println("активный сервер:", st.ActiveName)
	}
	return nil
}

// trim обрезает строку по числу рун, чтобы таблица не разъезжалась
// на именах с флагами и кириллицей.
func trim(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}

// runSelfUpdate проверяет обновление пакета и ставит его, если оно есть.
// Тот же путь, что и кнопка в интерфейсе, только из консоли.
func runSelfUpdate(cfg config.Config) error {
	ctx := context.Background()

	installed := selfupd.Installed()
	rel, err := selfupd.Latest(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("установлена: %s\nдоступна:    %s\n", orDashCLI(installed), rel.Version)

	if !selfupd.NeedsUpdate(installed, rel.Version) {
		fmt.Println("обновление не требуется")
		return nil
	}

	fmt.Printf("скачиваем %s…\n", rel.Asset)
	pkg, err := selfupd.Download(ctx, rel, cfg.StateDir, func(done, total int64) {
		if total > 0 {
			fmt.Printf("\rскачано %.0f%%", float64(done)/float64(total)*100)
		}
	})
	if err != nil {
		return err
	}
	fmt.Println()

	got, err := selfupd.PackageVersion(pkg)
	if err != nil {
		os.Remove(pkg)
		return err
	}
	if got != rel.Version {
		os.Remove(pkg)
		return fmt.Errorf("скачан пакет версии %s вместо %s — сборка ещё раздаётся, попробуйте через несколько минут",
			got, rel.Version)
	}
	fmt.Printf("пакет проверен: версия %s\n", got)

	updateLog := filepath.Join(filepath.Dir(cfg.LogFile), "kvas-web-update.log")
	if err := selfupd.Install(pkg, updateLog); err != nil {
		return err
	}
	fmt.Printf("установка запущена, журнал: %s\n", updateLog)
	fmt.Println("веб-интерфейс перезапустится через несколько секунд")
	return nil
}

func orDashCLI(v string) string {
	if v == "" {
		return "неизвестна"
	}
	return v
}
