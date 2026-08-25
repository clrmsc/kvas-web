package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

// Порты, по которым видно, что туннель действительно поднят.
const (
	vlessSOCKSPort    = 1097
	hysteriaSOCKSPort = 10808
)

type statusResponse struct {
	OK        bool   `json:"ok"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`     // vless | hysteria | none
	Failover  string `json:"failover"` // on | off | manual
	Hosts     int    `json:"hosts"`
	Tags      int    `json:"tags"`
	Adblock   bool   `json:"adblock"`
	VLESS     svc    `json:"vless"`
	Hysteria  svc    `json:"hysteria"`
	Interface string `json:"interface"`
}

type svc struct {
	Running bool `json:"running"`
	Tunnel  bool `json:"tunnel"` // слушается ли SOCKS-порт клиента
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conf, _ := kvas.Conf{Path: s.cfg.KvasConf}.GetAll()

	hosts, err := kvas.ReadList(s.cfg.HostsList)
	if err != nil {
		s.log.Warn("не прочитан список доменов", "err", err)
	}
	tags, err := kvas.ReadTags(s.cfg.TagsList)
	if err != nil {
		s.log.Warn("не прочитан список заквасок", "err", err)
	}

	resp := statusResponse{
		OK:        true,
		Package:   "kvas",
		Version:   kvas.PackageVersion("kvas"),
		Mode:      s.detectMode(conf),
		Failover:  s.failoverMode(),
		Hosts:     len(hosts),
		Tags:      len(tags),
		Adblock:   kvas.FileHasLine(s.cfg.DnsmasqConf, "addn-hosts=/opt/etc/adblock/ads.kvas.list"),
		Interface: conf["INFACE_ENT"],
		VLESS: svc{
			Running: kvas.ProcessRunning("xray"),
			Tunnel:  kvas.PortListening(vlessSOCKSPort),
		},
		Hysteria: svc{
			Running: kvas.ProcessRunning("hysteria"),
			Tunnel:  kvas.PortListening(hysteriaSOCKSPort),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// detectMode определяет активный клиент по настроенному интерфейсу,
// а если тот ни о чём не говорит — по запущенным процессам.
func (s *Server) detectMode(conf map[string]string) string {
	iface := conf["INFACE_ENT"]
	switch {
	case strings.Contains(iface, "Proxy21"), strings.Contains(strings.ToLower(iface), "vless"):
		return "vless"
	case strings.Contains(iface, "Proxy41"), strings.Contains(strings.ToLower(iface), "hysteria"):
		return "hysteria"
	}
	switch {
	case kvas.ProcessRunning("xray"):
		return "vless"
	case kvas.ProcessRunning("hysteria"):
		return "hysteria"
	}
	return "none"
}

func (s *Server) failoverMode() string {
	mode, err := kvas.Conf{Path: s.cfg.FailoverConf}.Get("FAILOVER_MODE")
	if err != nil || mode == "" {
		return "manual"
	}
	return mode
}

func (s *Server) handleVPNStatus(w http.ResponseWriter, r *http.Request) {
	conf, _ := kvas.Conf{Path: s.cfg.KvasConf}.GetAll()
	fo := kvas.Conf{Path: s.cfg.FailoverConf}
	interval, _ := fo.Get("CHECK_INTERVAL")
	threshold, _ := fo.Get("FAIL_THRESHOLD")
	primary, _ := fo.Get("PRIMARY")
	if primary == "" {
		primary = "vless"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"mode":      s.detectMode(conf),
		"interface": conf["INFACE_ENT"],
		"vless":     svc{Running: kvas.ProcessRunning("xray"), Tunnel: kvas.PortListening(vlessSOCKSPort)},
		"hysteria":  svc{Running: kvas.ProcessRunning("hysteria"), Tunnel: kvas.PortListening(hysteriaSOCKSPort)},
		"failover": map[string]any{
			"mode":      s.failoverMode(),
			"primary":   primary,
			"interval":  atoiOr(interval, 15),
			"threshold": atoiOr(threshold, 2),
		},
	})
}

func (s *Server) handleVPNSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Mode != "vless" && body.Mode != "hysteria" {
		writeError(w, http.StatusBadRequest, "режим должен быть vless или hysteria")
		return
	}
	out, err := s.kvas.Run(r.Context(), "vpn", "set", body.Mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось переключить туннель: "+out)
		return
	}
	s.log.Info("туннель переключён", "mode", body.Mode)
	writeOK(w, "переключено на "+body.Mode)
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}
