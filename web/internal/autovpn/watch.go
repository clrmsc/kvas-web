package autovpn

// watch.go — сторож туннеля. Проверок две, и они про разное:
//
//   - «своя сторона»: есть ли конфигурация, слушает ли xray порт, поднят ли
//     прокси-интерфейс роутера. Дёшево, выполняется всегда;
//   - «связь наружу»: реальный запрос через рабочий SOCKS. Только она видит,
//     что сервер подписки умер или его заблокировали, — со стороны роутера
//     в этот момент всё выглядит исправным.
//
// Вторая включается пользователем и умеет чинить туннель сама.

import (
	"context"
	"fmt"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/probe"
)

const (
	// liveTimeout — сколько ждём ответа проверочной страницы через туннель.
	liveTimeout = 8 * time.Second
	// maxRepairServers — сколько запасных серверов пробуем, прежде чем
	// признать, что дело не в конкретном сервере.
	maxRepairServers = 3
)

// WatchTunnel следит за туннелем, пока не отменят контекст.
//
// base — период базовой проверки; проверка связи идёт своим периодом из
// настроек, поэтому таймер пересчитывается на каждом шаге: пользователь
// может изменить период прямо в интерфейсе.
//
// Первый шаг — сразу: сервис как раз перезапускают после обновления, когда
// конфигурация туннеля могла и не пережить установку.
func (m *Manager) WatchTunnel(ctx context.Context, base time.Duration) {
	if base <= 0 {
		base = 5 * time.Minute
	}
	var lastLive time.Time
	m.watchStep(ctx, &lastLive)

	for {
		st := m.State()
		wait := base
		if st.LiveCheck && st.LiveInterval() < wait {
			wait = st.LiveInterval()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			m.watchStep(ctx, &lastLive)
		}
	}
}

func (m *Manager) watchStep(ctx context.Context, lastLive *time.Time) {
	if _, err := m.EnsureTunnel(ctx); err != nil {
		m.log.Error("не удалось восстановить туннель", "err", err)
		return
	}

	st := m.State()
	if !st.LiveCheck || st.ActiveKey == "" {
		return
	}
	if !lastLive.IsZero() && time.Since(*lastLive) < st.LiveInterval() {
		return
	}
	// Полная проверка подписки перезапускает xray и занимает порт — её
	// собственные замеры надёжнее, чем то, что увидит сторож посреди неё.
	if m.Running() {
		return
	}
	*lastLive = time.Now()
	m.liveVerify(ctx, st)
}

// liveVerify проверяет, ходит ли трафик наружу, и чинит туннель, если нет.
func (m *Manager) liveVerify(ctx context.Context, st State) {
	latency, err := m.liveProbe(ctx)
	if err == nil {
		m.recordLive(true, latency, "", 0)
		return
	}

	// У провайдера обрыв — туннель тут ни при чём, чинить нечего.
	if !m.internetAvailable(ctx) {
		m.log.Info("проверка связи пропущена: у роутера нет интернета")
		m.recordLive(false, 0, "у роутера нет интернета", 0)
		return
	}

	m.log.Warn("через туннель нет связи, восстанавливаем",
		"сервер", st.ActiveName, "err", err)

	// Сперва перезапускаем текущий сервер: чаще всего виноват не он,
	// а отвалившийся прокси-интерфейс или подвисший xray.
	if applyErr := m.Apply(ctx, st.ActiveKey); applyErr != nil {
		m.log.Error("не удалось переприменить текущий сервер",
			"сервер", st.ActiveName, "err", applyErr)
	} else if latency, err = m.liveProbe(ctx); err == nil {
		m.log.Info("связь восстановлена перезапуском текущего сервера",
			"сервер", st.ActiveName)
		m.recordLive(true, latency, "", 1)
		return
	}

	// Не помогло — значит дело в самом сервере. Переключаемся на запасной,
	// но только если пользователь вообще разрешил менять сервер: иначе
	// выходной адрес сменился бы у него за спиной.
	if !st.AutoApply {
		m.log.Warn("сервер не отвечает, автопереключение выключено",
			"сервер", st.ActiveName)
		m.recordLive(false, 0, "через туннель нет связи, автопереключение выключено", 0)
		return
	}

	for i, cand := range m.spareServers(st, maxRepairServers) {
		m.log.Info("пробуем запасной сервер", "сервер", cand.Name, "попытка", i+1)
		if applyErr := m.Apply(ctx, cand.Key); applyErr != nil {
			m.log.Warn("запасной сервер не применился", "сервер", cand.Name, "err", applyErr)
			continue
		}
		latency, err = m.liveProbe(ctx)
		if err == nil {
			m.log.Info("туннель переключён на запасной сервер", "сервер", cand.Name)
			m.recordLive(true, latency, "", 1)
			return
		}
		m.log.Warn("через запасной сервер связи тоже нет", "сервер", cand.Name, "err", err)
	}

	m.log.Error("восстановить связь не удалось", "err", err)
	m.recordLive(false, 0, fmt.Sprintf("через туннель нет связи: %v", err), 0)
}

// internetAvailable отвечает, есть ли у роутера интернет вообще.
func (m *Manager) internetAvailable(ctx context.Context) bool {
	if m.internetUp != nil {
		return m.internetUp(ctx)
	}
	return probe.InternetUp(ctx, 4*time.Second)
}

// liveProbe запрашивает проверочную страницу через рабочий SOCKS Кваса.
func (m *Manager) liveProbe(ctx context.Context) (time.Duration, error) {
	url := m.cfg.TunnelURL
	if url == "" {
		url = probe.DefaultOptions().TunnelURL
	}
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.ProxyPort)
	return probe.ProxyCheck(ctx, addr, url, liveTimeout)
}

// spareServers возвращает кандидатов на замену: рабочие по последней
// проверке серверы, кроме текущего, в порядке их места в рейтинге.
func (m *Manager) spareServers(st State, limit int) []probe.Result {
	out := make([]probe.Result, 0, limit)
	for _, r := range st.Results {
		if r.Key == st.ActiveKey || !r.Alive() {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out
}

// recordLive запоминает итог проверки связи. На диск пишем только при
// смене состояния: сторож срабатывает каждые несколько минут, а флеш
// роутера незачем изнашивать ради строчки в журнале.
func (m *Manager) recordLive(ok bool, latency time.Duration, errText string, repairs int) {
	m.mu.Lock()
	first := !m.liveSeen
	m.liveSeen = true
	changed := m.state.LiveOK != ok || m.state.LiveError != errText || m.state.LiveAt.IsZero()
	m.state.LiveOK = ok
	m.state.LiveAt = time.Now()
	m.state.LiveError = errText
	m.state.LiveRepairs += repairs
	if ok {
		m.state.LiveMS = float64(latency.Microseconds()) / 1000
	} else {
		m.state.LiveMS = 0
	}
	if repairs > 0 {
		changed = true
	}
	stateCopy := m.state
	m.mu.Unlock()

	// Успех отмечаем в журнале при первой проверке и при смене состояния:
	// иначе сторож исписал бы журнал одинаковыми строками каждые несколько
	// минут. Сразу после починки о ней уже сказано выше, не повторяемся.
	if ok && repairs == 0 && (first || changed) {
		m.log.Info("связь через туннель есть", "задержка_мс", int(latency.Milliseconds()))
	}
	if !changed {
		return
	}
	if err := saveState(m.cfg.SubscriptionFile(), stateCopy); err != nil {
		m.log.Warn("состояние подписки не сохранено", "err", err)
	}
}
