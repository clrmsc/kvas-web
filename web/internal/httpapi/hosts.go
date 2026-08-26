package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

func (s *Server) handleHostsList(w http.ResponseWriter, r *http.Request) {
	hosts, err := kvas.ReadList(s.cfg.HostsList)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать список: "+err.Error())
		return
	}
	sort.Strings(hosts)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hosts": hosts})
}

func (s *Server) handleHostAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	var body struct {
		Domain string `json:"domain"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	domain, err := kvas.NormalizeDomain(body.Domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.kvas.Run(r.Context(), "add", domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось добавить домен: "+out)
		return
	}
	s.log.Info("домен добавлен", "domain", domain)
	writeOK(w, "добавлен "+domain)
}

func (s *Server) handleHostDel(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	raw, err := url.PathUnescape(r.PathValue("domain"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректный адрес запроса")
		return
	}
	domain, err := kvas.NormalizeDomain(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.kvas.Run(r.Context(), "del", domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось удалить домен: "+out)
		return
	}
	// Пересобирать ipset вручную не нужно: kvas del сам обновляет таблицу
	// и перезапускает dnsmasq.
	s.log.Info("домен удалён", "domain", domain)
	writeOK(w, "удалён "+domain)
}

func (s *Server) handleHostsExport(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.cfg.HostsList)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="kvas.list"`)
	_, _ = w.Write(data)
}

// handleHostsImport принимает список доменов, чистит его и отдаёт прогресс
// импорта потоком server-sent events: на большом списке это минуты работы.
//
// По умолчанию домены добавляются к текущему списку. Команда CLI `kvas
// import` для этого не подходит: она заменяет список целиком, унося
// прежние домены в свой внутренний архив, — поэтому добавление идёт
// пачками через `kvas add`. Замена доступна отдельным флагом.
func (s *Server) handleHostsImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	var body struct {
		Domains string `json:"domains"`
		Replace bool   `json:"replace"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var valid []string
	var skipped []string
	for _, line := range strings.Split(body.Domains, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Терпимо относимся к спискам, скопированным из hosts или из браузера.
		line = strings.TrimPrefix(line, "0.0.0.0 ")
		line = strings.TrimPrefix(line, "127.0.0.1 ")
		if u, err := url.Parse(line); err == nil && u.Host != "" {
			line = u.Hostname()
		}
		d, err := kvas.NormalizeDomain(line)
		if err != nil {
			skipped = append(skipped, line)
			continue
		}
		valid = append(valid, d)
	}
	if len(valid) == 0 {
		writeError(w, http.StatusBadRequest, "в списке нет корректных доменов")
		return
	}

	// Список — единственное, что нельзя восстановить из подписки или
	// шаблона, поэтому перед изменением сохраняем копию.
	backup, err := s.backupHostsList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось сохранить копию списка: "+err.Error())
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

	send("start", map[string]any{
		"total":   len(valid),
		"skipped": len(skipped),
		"replace": body.Replace,
		"backup":  backup,
	})

	// Операция не должна обрываться, если пользователь закрыл вкладку:
	// прерванная замена оставила бы список в половинчатом состоянии.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()

	var runErr error
	if body.Replace {
		runErr = s.replaceHostsList(opCtx, valid, send)
	} else {
		runErr = s.appendHostsList(opCtx, valid, send)
	}
	if runErr != nil {
		send("error", map[string]string{
			"error":  runErr.Error(),
			"backup": backup,
		})
		return
	}

	s.log.Info("импорт доменов завершён",
		"добавлено", len(valid), "пропущено", len(skipped), "замена", body.Replace)
	send("done", map[string]any{"imported": len(valid), "skipped": skipped, "backup": backup})
}

// appendHostsList добавляет домены к существующему списку пачками.
func (s *Server) appendHostsList(ctx context.Context, domains []string,
	send func(string, any)) error {

	existing, err := kvas.ReadList(s.cfg.HostsList)
	if err != nil {
		return err
	}
	have := kvas.InSet(existing)

	var fresh []string
	for _, d := range domains {
		if !have[d] {
			fresh = append(fresh, d)
			have[d] = true // защита от повторов внутри самого списка
		}
	}
	if len(fresh) == 0 {
		send("line", map[string]string{"line": "все домены уже в списке"})
		return nil
	}

	send("line", map[string]string{
		"line": fmt.Sprintf("добавляем %d доменов (уже было %d)", len(fresh), len(existing)),
	})

	for start := 0; start < len(fresh); start += batchSize {
		end := min(start+batchSize, len(fresh))
		batch := fresh[start:end]
		args := append([]string{"add"}, batch...)
		if out, err := s.kvas.Run(ctx, args...); err != nil {
			return fmt.Errorf("не удалось добавить домены (%s…): %s", batch[0], out)
		}
		send("line", map[string]string{
			"line": fmt.Sprintf("добавлено %d из %d", end, len(fresh)),
		})
	}
	return nil
}

// replaceHostsList заменяет список целиком — тем самым `kvas import`,
// который уносит прежние домены в архив Кваса.
func (s *Server) replaceHostsList(ctx context.Context, domains []string,
	send func(string, any)) error {

	tmp, err := os.CreateTemp("", "kvas-import-*.list")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(domains, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	return s.kvas.RunStream(ctx, func(line string) {
		send("line", map[string]string{"line": line})
	}, "import", tmp.Name())
}

// backupHostsList складывает копию списка рядом с состоянием веб-интерфейса
// и возвращает путь к ней.
func (s *Server) backupHostsList() (string, error) {
	data, err := os.ReadFile(s.cfg.HostsList)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	path := filepath.Join(s.cfg.StateDir, "kvas.list.bak")
	if err := kvas.WriteFileAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
