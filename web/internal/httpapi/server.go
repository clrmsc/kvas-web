// Package httpapi реализует REST-интерфейс веб-морды Кваса и отдаёт SPA.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/auth"
	"github.com/clrmsc/kvas-web/web/internal/autovpn"
	"github.com/clrmsc/kvas-web/web/internal/config"
	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

const sessionCookie = "kvasweb_session"

// Server связывает конфигурацию, менеджер сессий и клиент CLI.
type Server struct {
	cfg     config.Config
	auth    *auth.Manager
	kvas    *kvas.Client
	autovpn *autovpn.Manager
	log     *slog.Logger
	static  fs.FS
	secure  bool // отдавать ли cookie с флагом Secure (включён TLS)
}

// timeNow вынесен переменной, чтобы тесты могли зафиксировать время.
var timeNow = time.Now

// New собирает сервер. static — файловая система с собранным SPA.
func New(cfg config.Config, am *auth.Manager, av *autovpn.Manager, log *slog.Logger, static fs.FS) *Server {
	return &Server{
		cfg:     cfg,
		auth:    am,
		kvas:    kvas.NewClient(cfg.KvasBin),
		autovpn: av,
		log:     log,
		static:  static,
		secure:  cfg.TLSCert != "" && cfg.TLSKey != "",
	}
}

// Handler возвращает готовый обработчик со всеми маршрутами.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Аутентификация — единственная часть, доступная без сессии.
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)

	// Всё остальное — только с действующей сессией.
	protected := http.NewServeMux()
	protected.HandleFunc("POST /api/auth/password", s.handleAuthChangePassword)
	protected.HandleFunc("GET /api/status", s.handleStatus)

	protected.HandleFunc("GET /api/hosts", s.handleHostsList)
	protected.HandleFunc("POST /api/hosts", s.handleHostAdd)
	protected.HandleFunc("DELETE /api/hosts/{domain}", s.handleHostDel)
	protected.HandleFunc("POST /api/hosts/import", s.handleHostsImport)
	protected.HandleFunc("GET /api/hosts/export", s.handleHostsExport)

	protected.HandleFunc("GET /api/tags", s.handleTagsList)
	protected.HandleFunc("POST /api/tags/{tag}/enable", s.handleTagEnable)
	protected.HandleFunc("POST /api/tags/{tag}/disable", s.handleTagDisable)

	protected.HandleFunc("GET /api/adblock", s.handleAdblockStatus)
	protected.HandleFunc("POST /api/adblock", s.handleAdblockToggle)
	protected.HandleFunc("GET /api/adblock/blocked", s.handleBlockedList)
	protected.HandleFunc("POST /api/adblock/blocked", s.handleBlockedAdd)
	protected.HandleFunc("DELETE /api/adblock/blocked/{domain}", s.handleBlockedDel)

	protected.HandleFunc("GET /api/routes", s.handleRoutesList)
	protected.HandleFunc("POST /api/routes", s.handleRouteAdd)
	protected.HandleFunc("DELETE /api/routes/{type}/{ip}", s.handleRouteDel)
	protected.HandleFunc("GET /api/routes/devices", s.handleRouteDevices)

	protected.HandleFunc("GET /api/subscription", s.handleSubscriptionGet)
	protected.HandleFunc("POST /api/subscription", s.handleSubscriptionSave)
	protected.HandleFunc("GET /api/subscription/servers", s.handleSubscriptionServers)
	protected.HandleFunc("POST /api/subscription/check", s.handleSubscriptionCheck)
	protected.HandleFunc("POST /api/subscription/apply", s.handleSubscriptionApply)

	protected.HandleFunc("GET /api/vpn", s.handleVPNStatus)
	protected.HandleFunc("POST /api/vpn/mode", s.handleVPNSet)

	protected.HandleFunc("POST /api/service/init", s.handleServiceInit)
	protected.HandleFunc("POST /api/service/update", s.handleServiceUpdate)
	protected.HandleFunc("POST /api/service/backup", s.handleServiceBackup)
	protected.HandleFunc("GET /api/logs", s.handleLogs)

	mux.Handle("/api/", s.requireAuth(protected))
	mux.Handle("/", s.staticHandler())

	return s.recoverPanic(s.logRequests(s.checkOrigin(mux)))
}

// requireAuth пропускает дальше только запросы с живой сессией.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.auth.Valid(c.Value) {
			writeError(w, http.StatusUnauthorized, "требуется вход")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkOrigin отбивает межсайтовые запросы: браузер обязан прислать Origin
// для небезопасных методов, и он должен совпадать с адресом сервиса.
// Вместе с SameSite=Strict это закрывает CSRF без отдельного токена.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				if !sameHost(origin, r.Host) {
					writeError(w, http.StatusForbidden, "запрос с чужого источника отклонён")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	return strings.EqualFold(origin, host)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Тело запроса не пишем: там пароли и списки доменов.
		s.log.Debug("запрос",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.code, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("паника при обработке запроса", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "внутренняя ошибка сервиса")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush пробрасывает сброс буфера — нужен для потоковой выдачи логов.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// staticHandler отдаёт SPA: неизвестные пути возвращают index.html,
// чтобы работала навигация внутри приложения.
func (s *Server) staticHandler() http.Handler {
	files := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'")

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(s.static, path); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeOK(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

// decodeJSON читает тело запроса с ограничением размера, чтобы кривой
// клиент не съел память роутера.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "не удалось разобрать запрос: "+err.Error())
		return false
	}
	return true
}
