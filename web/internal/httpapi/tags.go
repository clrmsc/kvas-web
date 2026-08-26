package httpapi

import (
	"context"
	"net/http"
	"net/url"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

// batchSize ограничивает длину командной строки: закваска может содержать
// сотни доменов, а argv на роутере не резиновый.
const batchSize = 40

type tagView struct {
	Name    string      `json:"name"`
	Domains []tagDomain `json:"domains"`
	Enabled bool        `json:"enabled"` // все домены закваски уже в списке
}

type tagDomain struct {
	Name   string `json:"name"`
	InList bool   `json:"in_list"`
}

func (s *Server) handleTagsList(w http.ResponseWriter, r *http.Request) {
	tags, err := kvas.ReadTags(s.tagsFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать закваски: "+err.Error())
		return
	}
	hosts, err := kvas.ReadList(s.cfg.HostsList)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "не удалось прочитать список доменов: "+err.Error())
		return
	}
	inList := kvas.InSet(hosts)

	views := make([]tagView, 0, len(tags))
	for _, t := range tags {
		v := tagView{Name: t.Name, Domains: make([]tagDomain, 0, len(t.Domains)), Enabled: len(t.Domains) > 0}
		for _, d := range t.Domains {
			present := inList[d]
			if !present {
				v.Enabled = false
			}
			v.Domains = append(v.Domains, tagDomain{Name: d, InList: present})
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tags": views})
}

func (s *Server) handleTagEnable(w http.ResponseWriter, r *http.Request) {
	s.applyTag(w, r, true)
}

func (s *Server) handleTagDisable(w http.ResponseWriter, r *http.Request) {
	s.applyTag(w, r, false)
}

// applyTag включает или выключает закваску целиком. CLI Кваса не умеет
// применять секцию одной командой, поэтому домены передаются пачками
// в kvas add / kvas del.
func (s *Server) applyTag(w http.ResponseWriter, r *http.Request, enable bool) {
	if !s.requireSetup(w) {
		return
	}
	raw, err := url.PathUnescape(r.PathValue("tag"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "некорректное имя закваски")
		return
	}
	name, err := kvas.NormalizeName(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tags, err := kvas.ReadTags(s.tagsFile())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var domains []string
	found := false
	for _, t := range tags {
		if t.Name == name {
			found = true
			for _, d := range t.Domains {
				if norm, err := kvas.NormalizeDomain(d); err == nil {
					domains = append(domains, norm)
				}
			}
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "закваска не найдена")
		return
	}
	if len(domains) == 0 {
		writeError(w, http.StatusBadRequest, "в закваске нет корректных доменов")
		return
	}

	verb := "add"
	if !enable {
		verb = "del"
	}
	if err := s.runBatched(r.Context(), verb, domains); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("закваска применена", "tag", name, "enable", enable, "domains", len(domains))
	if enable {
		writeOK(w, "закваска «"+name+"» включена")
		return
	}
	writeOK(w, "закваска «"+name+"» выключена")
}

func (s *Server) runBatched(ctx context.Context, verb string, domains []string) error {
	for start := 0; start < len(domains); start += batchSize {
		end := min(start+batchSize, len(domains))
		args := append([]string{verb}, domains[start:end]...)
		if out, err := s.kvas.Run(ctx, args...); err != nil {
			s.log.Warn("пачка доменов обработана с ошибкой", "verb", verb, "out", out, "err", err)
			return err
		}
	}
	return nil
}
