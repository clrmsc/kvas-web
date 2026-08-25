package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

// adblockDirective — строка dnsmasq, которой включается блокировка рекламы.
const adblockDirective = "addn-hosts=/opt/etc/adblock/ads.kvas.list"

func (s *Server) handleAdblockStatus(w http.ResponseWriter, r *http.Request) {
	blocked, err := kvas.ReadList(s.cfg.AdblockList)
	if err != nil {
		s.log.Warn("не прочитан список блокировок", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"enabled": kvas.FileHasLine(s.cfg.DnsmasqConf, adblockDirective),
		"blocked": len(blocked),
	})
}

func (s *Server) handleAdblockToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	verb := "off"
	if body.Enabled {
		verb = "on"
	}
	// У CLI команда adblock on спрашивает подтверждение, поэтому директиву
	// dnsmasq правим сами и перезапускаем резолвер.
	var err error
	if body.Enabled {
		err = kvas.AppendLine(s.cfg.DnsmasqConf, adblockDirective)
	} else {
		err = kvas.RemoveLines(s.cfg.DnsmasqConf, adblockDirective)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось изменить dnsmasq.conf: "+err.Error())
		return
	}
	if err := restartResolver(); err != nil {
		writeError(w, http.StatusInternalServerError, "резолвер не перезапустился: "+err.Error())
		return
	}
	s.log.Info("блокировка рекламы переключена", "state", verb)
	if body.Enabled {
		writeOK(w, "блокировка рекламы включена")
		return
	}
	writeOK(w, "блокировка рекламы выключена")
}

func (s *Server) handleBlockedList(w http.ResponseWriter, r *http.Request) {
	list, err := kvas.ReadList(s.cfg.AdblockList)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.Strings(list)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": list})
}

func (s *Server) handleBlockedAdd(w http.ResponseWriter, r *http.Request) {
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
	out, err := s.kvas.Run(r.Context(), "adblock", "add", domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось заблокировать: "+out)
		return
	}
	s.log.Info("домен заблокирован", "domain", domain)
	writeOK(w, "заблокирован "+domain)
}

func (s *Server) handleBlockedDel(w http.ResponseWriter, r *http.Request) {
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
	out, err := s.kvas.Run(r.Context(), "adblock", "del", domain)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось разблокировать: "+out)
		return
	}
	s.log.Info("домен разблокирован", "domain", domain)
	writeOK(w, "разблокирован "+domain)
}

// resolverServices — init-скрипты резолверов в порядке предпочтения.
// На части установок вместо dnsmasq работает AdGuard Home.
var resolverServices = []string{
	"/opt/etc/init.d/S56dnsmasq",
	"/opt/etc/init.d/S99adguardhome",
}

// restartResolver перезапускает тот резолвер, который установлен.
// Отсутствие обоих скриптов — не ошибка записи конфигурации: новые
// правила подхватятся при следующем запуске службы.
func restartResolver() error {
	for _, svc := range resolverServices {
		if _, err := os.Stat(svc); err != nil {
			continue
		}
		if err := exec.Command(svc, "restart").Run(); err != nil {
			return fmt.Errorf("%s restart: %w", filepath.Base(svc), err)
		}
		return nil
	}
	return nil
}
