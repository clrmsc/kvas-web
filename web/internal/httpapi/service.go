package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// handleServiceInit пересобирает ipset и перезапускает службы Кваса.
func (s *Server) handleServiceInit(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	s.streamCommand(w, r, "init")
}

// handleServiceUpdate обновляет списки доменов из внешних источников.
func (s *Server) handleServiceUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	s.streamCommand(w, r, "update")
}

// handleServiceBackup создаёт резервную копию настроек Кваса.
func (s *Server) handleServiceBackup(w http.ResponseWriter, r *http.Request) {
	out, err := s.kvas.Run(r.Context(), "backup")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "резервная копия не создана: "+out)
		return
	}
	s.log.Info("создана резервная копия")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "резервная копия создана", "output": out})
}

// streamCommand выполняет долгую команду CLI, передавая вывод в браузер
// по мере появления строк.
func (s *Server) streamCommand(w http.ResponseWriter, r *http.Request, args ...string) {
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

	send("start", map[string]string{"command": strings.Join(args, " ")})
	if err := s.kvas.RunStream(r.Context(), func(line string) {
		send("line", map[string]string{"line": line})
	}, args...); err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	send("done", map[string]bool{"ok": true})
}

// handleLogs отдаёт хвост журнала веб-сервиса — этого достаточно, чтобы
// понять, почему не сработала операция, не заходя по SSH.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			lines = n
		}
	}
	data, err := os.ReadFile(s.cfg.LogFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": []string{}})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": all})
}
