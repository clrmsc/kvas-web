package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/clrmsc/kvas-web/web/internal/autovpn"
	"github.com/clrmsc/kvas-web/web/internal/probe"
)

// subscriptionView — состояние подписки в том виде, в каком его показывает
// интерфейс. Полная ссылка наружу не отдаётся: в ней токен доступа.
type subscriptionView struct {
	OK         bool           `json:"ok"`
	Configured bool           `json:"configured"`
	URLMasked  string         `json:"url_masked"`
	Enabled    bool           `json:"enabled"`
	AutoApply  bool           `json:"auto_apply"`
	CheckTime  string         `json:"check_time"`
	SpeedTopN  int            `json:"speed_top_n"`
	LastCheck  string         `json:"last_check,omitempty"`
	NextCheck  string         `json:"next_check,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
	ActiveKey  string         `json:"active_key,omitempty"`
	ActiveName string         `json:"active_name,omitempty"`
	AppliedAt  string         `json:"applied_at,omitempty"`
	Results    []probe.Result `json:"results"`
	// History — по дням для каждого сервера, ключ — адрес:порт.
	History map[string][]autovpn.HistoryPoint `json:"history,omitempty"`
}

func (s *Server) subscriptionView() subscriptionView {
	st := s.autovpn.State()
	v := subscriptionView{
		OK:         true,
		Configured: st.Configured(),
		URLMasked:  autovpn.MaskURL(st.URL),
		Enabled:    st.Enabled,
		AutoApply:  st.AutoApply,
		CheckTime:  st.CheckTime,
		SpeedTopN:  st.SpeedTopN,
		LastError:  st.LastError,
		ActiveKey:  st.ActiveKey,
		ActiveName: st.ActiveName,
		Results:    st.Results,
		History:    st.History,
	}
	if v.Results == nil {
		v.Results = []probe.Result{}
	}
	if !st.LastCheck.IsZero() {
		v.LastCheck = st.LastCheck.Format("2006-01-02 15:04")
	}
	if !st.AppliedAt.IsZero() {
		v.AppliedAt = st.AppliedAt.Format("2006-01-02 15:04")
	}
	if st.Enabled && st.Configured() {
		v.NextCheck = st.NextRun(timeNow()).Format("2006-01-02 15:04")
	}
	return v
}

func (s *Server) handleSubscriptionGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.subscriptionView())
}

func (s *Server) handleSubscriptionSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL       *string `json:"url"`
		Enabled   *bool   `json:"enabled"`
		AutoApply *bool   `json:"auto_apply"`
		CheckTime *string `json:"check_time"`
		SpeedTopN *int    `json:"speed_top_n"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, err := s.autovpn.UpdateSettings(autovpn.Settings{
		URL:       body.URL,
		Enabled:   body.Enabled,
		AutoApply: body.AutoApply,
		CheckTime: body.CheckTime,
		SpeedTopN: body.SpeedTopN,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Саму ссылку в журнал не пишем — это секрет.
	s.log.Info("настройки подписки изменены")
	writeJSON(w, http.StatusOK, s.subscriptionView())
}

// handleSubscriptionServers показывает состав подписки без проверки:
// удобно убедиться, что ссылка рабочая.
func (s *Server) handleSubscriptionServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.autovpn.Servers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	type item struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	list := make([]item, 0, len(servers))
	for _, srv := range servers {
		list = append(list, item{Key: srv.Key(), Name: srv.Name, Address: srv.Address, Port: srv.Port})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "servers": list})
}

// handleSubscriptionCheck запускает проверку и отдаёт ход выполнения
// потоком: полная проверка нескольких десятков серверов занимает минуты.
func (s *Server) handleSubscriptionCheck(w http.ResponseWriter, r *http.Request) {
	run, err := s.autovpn.StartCheck()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
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

	send("start", map[string]bool{"ok": true})

	// Проверка идёт в фоне: если пользователь закроет вкладку, она всё
	// равно доведётся до конца и переключит туннель.
	for res := range run.Events {
		select {
		case <-r.Context().Done():
			s.log.Info("клиент отключился, проверка продолжается в фоне")
			return
		default:
		}
		send("result", res)
	}

	st := s.autovpn.State()
	if st.LastError != "" {
		send("error", map[string]string{"error": st.LastError})
	}
	send("done", map[string]any{
		"results": st.Results,
		"state":   s.subscriptionView(),
	})
}

func (s *Server) handleSubscriptionApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "не указан сервер")
		return
	}
	if err := s.autovpn.Apply(r.Context(), body.Key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"msg":   "туннель переключён",
		"state": s.subscriptionView(),
	})
}
