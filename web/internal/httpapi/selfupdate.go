package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/selfupd"
)

// handleUpdateStatus показывает установленную версию пакета и, по запросу,
// выложенную в последнем релизе.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	installed := selfupd.Installed()
	resp := map[string]any{
		"ok":        true,
		"installed": installed,
	}

	if r.URL.Query().Get("check") == "1" {
		rel, err := selfupd.Latest(r.Context())
		if err != nil {
			resp["check_error"] = err.Error()
		} else {
			resp["latest"] = rel.Version
			resp["asset"] = rel.Asset
			resp["update_available"] = selfupd.NeedsUpdate(installed, rel.Version)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateInstall скачивает пакет и передаёт установку отдельному
// процессу: она заменит в том числе этот бинарник, и веб-интерфейс на
// минуту станет недоступен.
func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	line := func(format string, args ...any) {
		send("line", map[string]string{"line": fmt.Sprintf(format, args...)})
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Minute)
	defer cancel()

	installed := selfupd.Installed()
	send("start", map[string]any{"installed": installed})

	rel, err := selfupd.Latest(ctx)
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	line("установлена %s, доступна %s", orDash(installed), rel.Version)

	if !selfupd.NeedsUpdate(installed, rel.Version) {
		send("done", map[string]any{"updated": false, "msg": "уже установлена последняя версия"})
		return
	}

	// Качаем на диск рядом с состоянием: /tmp на роутере — оперативная память.
	line("скачиваем %s", rel.Asset)
	pkg, err := selfupd.Download(ctx, rel, s.cfg.StateDir, func(done, total int64) {
		if total > 0 {
			line("скачано %.0f%%", float64(done)/float64(total)*100)
		}
	})
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}

	// Раздача GitHub какое-то время отдаёт прежний файл, поэтому сверяем
	// версию внутри пакета: иначе молча переустановили бы то же самое.
	got, err := selfupd.PackageVersion(pkg)
	if err != nil {
		os.Remove(pkg)
		send("error", map[string]string{"error": err.Error()})
		return
	}
	if got != rel.Version {
		os.Remove(pkg)
		send("error", map[string]string{"error": fmt.Sprintf(
			"скачан пакет версии %s вместо %s — сборка ещё раздаётся, попробуйте через несколько минут",
			got, rel.Version)})
		return
	}
	line("пакет проверен: версия %s", got)

	updateLog := filepath.Join(filepath.Dir(s.cfg.LogFile), "kvas-web-update.log")
	if err := selfupd.Install(pkg, updateLog); err != nil {
		send("error", map[string]string{"error": "не удалось запустить установку: " + err.Error()})
		return
	}

	s.log.Info("запущено обновление пакета", "с", installed, "на", rel.Version)
	line("установка запущена, веб-интерфейс перезапустится")
	send("done", map[string]any{
		"updated": true,
		"version": rel.Version,
		"log":     updateLog,
		"msg":     "обновление устанавливается, страница перезагрузится сама",
	})
}
