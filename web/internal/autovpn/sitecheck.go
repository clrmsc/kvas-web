package autovpn

// sitecheck.go — «проверить, через какие серверы открывается этот сайт».
// Туннель может быть исправен, а отдельный сервис через него не идти:
// часть серверов не пускают к Meta, часть — к банкам.

import (
	"context"

	"github.com/clrmsc/kvas-web/web/internal/probe"
	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// SiteRun — идущая проверка адреса.
type SiteRun struct {
	URL    string
	Events <-chan probe.SiteResult
}

// StartSiteCheck запускает проверку в фоне. Как и обычная проверка
// подписки, она поднимает xray на каждый сервер, поэтому одновременно
// выполняется только одна.
func (m *Manager) StartSiteCheck(raw string) (*SiteRun, error) {
	target, err := probe.NormalizeSiteURL(raw)
	if err != nil {
		return nil, err
	}

	m.runningMu.Lock()
	if m.running {
		m.runningMu.Unlock()
		return nil, ErrCheckInProgress
	}
	m.running = true
	m.runningMu.Unlock()

	events := make(chan probe.SiteResult, 64)
	go func() {
		ctx, cancel := probe.SiteCheckContext(context.Background())
		defer cancel()
		defer close(events)
		defer func() {
			m.runningMu.Lock()
			m.running = false
			m.runningMu.Unlock()
		}()

		servers, err := m.Servers(ctx)
		if err != nil {
			m.log.Error("подписка не скачана", "err", err)
			return
		}
		// Порядок — как в последней проверке: сначала те, что считаются
		// лучшими, чтобы пригодный сервер нашёлся в первых строках.
		servers = m.sortByRank(servers)

		m.log.Info("проверка адреса через серверы подписки", "адрес", target, "серверов", len(servers))
		opt := probe.DefaultOptions()
		opt.XrayBin = m.xrayBin()
		opt.TunnelURL = target

		probe.SiteCheck(ctx, servers, opt, func(r probe.SiteResult) {
			select {
			case events <- r:
			default:
			}
		})
		m.log.Info("проверка адреса завершена", "адрес", target)
	}()

	return &SiteRun{URL: target, Events: events}, nil
}

// sortByRank переставляет серверы в порядке последней проверки: сначала
// те, что оказались лучшими, затем новые, которых в ней ещё не было.
func (m *Manager) sortByRank(servers []subscription.Server) []subscription.Server {
	st := m.State()
	rank := make(map[string]int, len(st.Results))
	for i, r := range st.Results {
		rank[r.Key] = i
	}

	sorted := make([]subscription.Server, 0, len(servers))
	used := make(map[string]bool, len(servers))
	for pos := 0; pos < len(st.Results); pos++ {
		for _, s := range servers {
			if used[s.Key()] {
				continue
			}
			if r, ok := rank[s.Key()]; ok && r == pos {
				sorted = append(sorted, s)
				used[s.Key()] = true
			}
		}
	}
	for _, s := range servers {
		if !used[s.Key()] {
			sorted = append(sorted, s)
		}
	}
	return sorted
}
