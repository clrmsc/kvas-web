package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

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
func (s *Server) handleHostsImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	var body struct {
		Domains string `json:"domains"`
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

	tmp, err := os.CreateTemp("", "kvas-import-*.list")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(valid, "\n") + "\n"); err != nil {
		tmp.Close()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp.Close()

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

	send("start", map[string]any{"total": len(valid), "skipped": len(skipped)})
	runErr := s.kvas.RunStream(r.Context(), func(line string) {
		send("line", map[string]string{"line": line})
	}, "import", tmp.Name())
	if runErr != nil {
		send("error", map[string]string{"error": runErr.Error()})
		return
	}
	s.log.Info("импорт доменов завершён", "count", len(valid), "skipped", len(skipped))
	send("done", map[string]any{"imported": len(valid), "skipped": skipped})
}
