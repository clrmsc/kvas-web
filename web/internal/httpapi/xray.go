package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
	"github.com/clrmsc/kvas-web/web/internal/xrayupd"
)

// handleXrayStatus показывает установленную версию клиента и последнюю
// опубликованную. Запрос к GitHub делается только по нажатию кнопки, чтобы
// страница состояния не зависела от внешней сети.
func (s *Server) handleXrayStatus(w http.ResponseWriter, r *http.Request) {
	bin := s.xrayBin()
	installed := kvas.XrayVersion(bin)

	resp := map[string]any{
		"ok":        true,
		"installed": installed,
		"path":      bin,
	}

	if r.URL.Query().Get("check") == "1" {
		rel, err := xrayupd.Latest(r.Context())
		if err != nil {
			resp["check_error"] = err.Error()
		} else {
			resp["latest"] = rel.Version
			resp["published"] = rel.Published
			resp["asset"] = rel.AssetName
			resp["size_bytes"] = rel.Size
			resp["update_available"] = xrayupd.NeedsUpdate(installed, rel.Version)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleXrayUpdate скачивает официальную сборку, подменяет бинарник и
// перезапускает туннель, отдавая ход работы потоком. Прежний файл
// сохраняется: если туннель не поднимется, всё возвращается назад.
func (s *Server) handleXrayUpdate(w http.ResponseWriter, r *http.Request) {
	bin := s.xrayBin()
	if bin == "" {
		writeError(w, http.StatusConflict, "xray не найден на роутере")
		return
	}

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

	// Замена бинарника не должна обрываться вместе с вкладкой: на середине
	// это оставило бы роутер без рабочего xray.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Minute)
	defer cancel()

	installed := kvas.XrayVersion(bin)
	send("start", map[string]any{"installed": installed})

	rel, err := xrayupd.Latest(ctx)
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	line("установлен %s, последний выпуск %s (%s)", orDash(installed), rel.Version, rel.Published)

	if !xrayupd.NeedsUpdate(installed, rel.Version) {
		send("done", map[string]any{"updated": false, "installed": installed,
			"msg": "уже установлена последняя версия"})
		return
	}

	// Качаем рядом с бинарником: /tmp на роутере — это оперативная память.
	dir := filepath.Dir(bin)
	line("скачиваем %s (%.1f МБ)", rel.AssetName, float64(rel.Size)/1024/1024)
	archive, err := xrayupd.Download(ctx, rel, dir, func(done, total int64) {
		if total > 0 {
			line("скачано %.0f%%", float64(done)/float64(total)*100)
		}
	})
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(archive)
	line("контрольная сумма совпала")

	newBin := bin + ".new"
	if err := xrayupd.ExtractBinary(archive, newBin); err != nil {
		os.Remove(newBin)
		send("error", map[string]string{"error": err.Error()})
		return
	}
	defer os.Remove(newBin)

	version, err := runVersion(ctx, newBin)
	if err != nil {
		send("error", map[string]string{"error": "скачанный xray не запускается: " + err.Error()})
		return
	}
	line("скачанный файл работает, версия %s", version)

	backup := bin + ".bak"
	if err := copyFile(bin, backup); err != nil {
		send("error", map[string]string{"error": "не удалось сохранить прежний xray: " + err.Error()})
		return
	}
	line("прежний клиент сохранён в %s", backup)

	if err := os.Rename(newBin, bin); err != nil {
		send("error", map[string]string{"error": "не удалось заменить файл: " + err.Error()})
		return
	}
	_ = os.Chmod(bin, 0o755)

	line("перезапускаем туннель")
	if err := s.autovpn.RestartTunnel(ctx); err != nil {
		line("туннель не поднялся: %v", err)
		line("возвращаем прежний клиент")
		if rerr := copyFile(backup, bin); rerr != nil {
			send("error", map[string]string{"error": "откат не удался: " + rerr.Error()})
			return
		}
		_ = os.Chmod(bin, 0o755)
		if rerr := s.autovpn.RestartTunnel(ctx); rerr != nil {
			send("error", map[string]string{"error": "прежний клиент возвращён, но туннель не поднялся: " + rerr.Error()})
			return
		}
		send("error", map[string]string{"error": "обновление отменено, работает прежняя версия " + installed})
		return
	}

	s.log.Info("xray обновлён", "было", installed, "стало", version)
	send("done", map[string]any{"updated": true, "installed": version,
		"msg": "xray обновлён до " + version})
}

// runVersion спрашивает у бинарника его версию — заодно проверяет, что он
// вообще запускается на этом процессоре.
func runVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", err
	}
	if v := kvas.StripANSI(string(out)); v != "" {
		if version := firstVersionField(v); version != "" {
			return version, nil
		}
	}
	return "", fmt.Errorf("не удалось разобрать вывод version")
}

func firstVersionField(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return ""
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return kvas.WriteFileAtomic(dst, data, 0o755)
}

func orDash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}
