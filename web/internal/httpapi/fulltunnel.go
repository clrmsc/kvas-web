package httpapi

import "net/http"

// handleFullTunnelStatus отдаёт и выбор пользователя, и действительное
// положение правил: они расходятся, если таблицы пересоздали со стороны.
func (s *Server) handleFullTunnelStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.fulltunnel.Status()
	body := map[string]any{
		"ok":       true,
		"enabled":  st.Enabled,
		"applied":  st.Applied,
		"iface":    st.Iface,
		"excluded": st.Excluded,
	}
	if err != nil {
		body["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleFullTunnelSet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.fulltunnel.Set(body.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("режим полного туннеля изменён", "включён", body.Enabled)
	if body.Enabled {
		writeOK(w, "весь трафик идёт через туннель")
		return
	}
	writeOK(w, "через туннель снова идут только домены из списка")
}
