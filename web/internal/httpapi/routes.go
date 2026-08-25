package httpapi

import (
	"bufio"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/clrmsc/kvas-web/web/internal/keenetic"
	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

// Ключи kvas.conf, в которых хранятся правила маршрутизации по IP.
// Значения — адреса, разделённые знаком «+».
var routeKeys = map[string]string{
	"full":    "route_full_ip",     // весь трафик устройства в туннель
	"list":    "route_by_list_ip",  // только домены из списка
	"exclude": "route_excluded_ip", // мимо туннеля целиком
}

type routeEntry struct {
	Type   string `json:"type"`
	IP     string `json:"ip"`
	Device string `json:"device,omitempty"`
}

func (s *Server) handleRoutesList(w http.ResponseWriter, r *http.Request) {
	conf, err := kvas.Conf{Path: s.cfg.KvasConf}.GetAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать kvas.conf: "+err.Error())
		return
	}
	names := s.deviceNames(r)

	routes := []routeEntry{}
	for _, typ := range []string{"full", "list", "exclude"} {
		for _, ip := range splitPlus(conf[routeKeys[typ]]) {
			routes = append(routes, routeEntry{Type: typ, IP: ip, Device: names[ip]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"routes": routes,
		"guest":  splitComma(conf["INFACE_GUEST_ENT"]),
	})
}

func (s *Server) handleRouteAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
		IP   string `json:"ip"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	key, ok := routeKeys[body.Type]
	if !ok {
		writeError(w, http.StatusBadRequest, "тип должен быть full, list или exclude")
		return
	}
	ip, err := kvas.NormalizeIP(body.IP)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	conf := kvas.Conf{Path: s.cfg.KvasConf}
	current, _ := conf.Get(key)
	items := splitPlus(current)
	for _, it := range items {
		if it == ip {
			writeOK(w, "правило уже существует")
			return
		}
	}
	// Один и тот же адрес в двух режимах — противоречивая конфигурация,
	// поэтому сначала убираем его из остальных списков.
	for otherType, otherKey := range routeKeys {
		if otherType == body.Type {
			continue
		}
		if v, _ := conf.Get(otherKey); contains(splitPlus(v), ip) {
			if err := conf.Set(otherKey, joinPlus(remove(splitPlus(v), ip))); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	if err := conf.Set(key, joinPlus(append(items, ip))); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out, err := s.kvas.Run(r.Context(), "route", "refresh"); err != nil {
		writeError(w, http.StatusInternalServerError, "правило записано, но маршруты не применились: "+out)
		return
	}
	s.log.Info("правило маршрутизации добавлено", "type", body.Type, "ip", ip)
	writeOK(w, "правило добавлено: "+ip)
}

func (s *Server) handleRouteDel(w http.ResponseWriter, r *http.Request) {
	key, ok := routeKeys[r.PathValue("type")]
	if !ok {
		writeError(w, http.StatusBadRequest, "тип должен быть full, list или exclude")
		return
	}
	raw, err := url.PathUnescape(r.PathValue("ip"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректный адрес запроса")
		return
	}
	ip, err := kvas.NormalizeIP(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	conf := kvas.Conf{Path: s.cfg.KvasConf}
	current, _ := conf.Get(key)
	items := splitPlus(current)
	if !contains(items, ip) {
		writeOK(w, "правило не найдено")
		return
	}
	if err := conf.Set(key, joinPlus(remove(items, ip))); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out, err := s.kvas.Run(r.Context(), "route", "refresh"); err != nil {
		writeError(w, http.StatusInternalServerError, "правило удалено, но маршруты не применились: "+out)
		return
	}
	s.log.Info("правило маршрутизации удалено", "type", r.PathValue("type"), "ip", ip)
	writeOK(w, "правило удалено: "+ip)
}

// handleRouteDevices отдаёт список устройств сети, чтобы правило можно было
// выбрать из списка, а не вводить IP руками.
func (s *Server) handleRouteDevices(w http.ResponseWriter, r *http.Request) {
	names := s.deviceNames(r)
	type dev struct {
		IP   string `json:"ip"`
		Name string `json:"name"`
	}
	devices := make([]dev, 0, len(names))
	for ip, name := range names {
		devices = append(devices, dev{IP: ip, Name: name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "devices": devices})
}

// deviceNames собирает соответствие IP → имя: сначала из аренд DHCP,
// затем дополняет соседями из таблицы ARP.
func (s *Server) deviceNames(r *http.Request) map[string]string {
	names := make(map[string]string)
	leases, err := keenetic.New(s.cfg.RCIAddr).DHCPLeases(r.Context())
	if err != nil {
		s.log.Debug("RCI недоступен", "err", err)
	}
	for _, l := range leases {
		if l.IP != "" {
			names[l.IP] = l.Name
		}
	}
	for _, ip := range arpNeighbours() {
		if _, ok := names[ip]; !ok {
			names[ip] = ""
		}
	}
	return names
}

// arpNeighbours читает /proc/net/arp — устройства без записи в DHCP тоже
// должны быть видны в списке.
func arpNeighbours() []string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close()
	var ips []string
	sc := bufio.NewScanner(f)
	sc.Scan() // заголовок
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[2] == "0x0" {
			continue
		}
		if ip, err := kvas.NormalizeIP(fields[0]); err == nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func splitPlus(v string) []string  { return splitClean(v, "+") }
func splitComma(v string) []string { return splitClean(v, ",") }

func splitClean(v, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(v, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinPlus(items []string) string { return strings.Join(items, "+") }

func contains(items []string, v string) bool {
	for _, it := range items {
		if it == v {
			return true
		}
	}
	return false
}

func remove(items []string, v string) []string {
	out := items[:0]
	for _, it := range items {
		if it != v {
			out = append(out, it)
		}
	}
	return out
}
