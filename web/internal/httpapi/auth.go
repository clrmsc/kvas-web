package httpapi

import (
	"errors"
	"net"
	"net/http"

	"github.com/clrmsc/kvas-web/web/internal/auth"
)

type credentials struct {
	Password string `json:"password"`
}

type passwordChange struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	authed := false
	if c, err := r.Cookie(sessionCookie); err == nil {
		authed = s.auth.Valid(c.Value)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"has_password":     s.auth.HasPassword(),
		"authenticated":    authed,
		"min_password_len": auth.MinPasswordLen,
	})
}

// handleAuthSetup задаёт первый пароль. Работает только пока пароля нет и
// только для запросов из локальной сети: если интерфейс по недосмотру
// оказался доступен снаружи, свежую установку не перехватят из интернета.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if s.auth.HasPassword() {
		writeError(w, http.StatusConflict, "пароль уже задан, войдите и смените его в настройках")
		return
	}
	if !isLocalAddr(r.RemoteAddr) {
		s.log.Warn("попытка первичной настройки извне локальной сети", "addr", r.RemoteAddr)
		writeError(w, http.StatusForbidden,
			"первый пароль можно задать только из домашней сети роутера")
		return
	}
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	if err := s.auth.SetPassword(c.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("задан пароль администратора")
	s.loginAndSetCookie(w, c.Password)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decodeJSON(w, r, &c) {
		return
	}
	if !s.auth.HasPassword() {
		writeError(w, http.StatusConflict, "пароль ещё не задан")
		return
	}
	s.loginAndSetCookie(w, c.Password)
}

func (s *Server) loginAndSetCookie(w http.ResponseWriter, password string) {
	id, err := s.auth.Verify(password)
	if err != nil {
		var locked auth.ErrLocked
		switch {
		case errors.As(err, &locked):
			writeError(w, http.StatusTooManyRequests, locked.Error())
		case errors.Is(err, auth.ErrWrongPassword):
			s.log.Warn("неудачная попытка входа")
			writeError(w, http.StatusUnauthorized, "неверный пароль")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeOK(w, "выход выполнен")
}

// handleAuthChangePassword меняет пароль и завершает все сессии, включая
// текущую: после смены пароля нужно войти заново.
func (s *Server) handleAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	var p passwordChange
	if !decodeJSON(w, r, &p) {
		return
	}
	if _, err := s.auth.Verify(p.Current); err != nil {
		writeError(w, http.StatusUnauthorized, "текущий пароль неверен")
		return
	}
	if err := s.auth.SetPassword(p.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auth.RevokeAll()
	s.log.Info("пароль администратора изменён")
	writeOK(w, "пароль изменён, войдите заново")
}

// isLocalAddr сообщает, пришёл ли запрос из домашней сети или с самого
// роутера. Адреса из интернета сюда попасть не должны.
func isLocalAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
