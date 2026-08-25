package autovpn

import (
	"context"
	"time"
)

// RunScheduler раз в сутки в назначенное время проверяет серверы подписки.
// Возвращается только при отмене контекста, поэтому вызывается из горутины.
func (m *Manager) RunScheduler(ctx context.Context) {
	// Пересчитываем ближайший запуск не реже раза в минуту: пользователь
	// может изменить время проверки прямо в интерфейсе.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	last := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			st := m.State()
			if !st.Enabled || !st.Configured() {
				continue
			}
			if !dueNow(st, now, last) {
				continue
			}

			last = now
			m.log.Info("суточная проверка серверов подписки по расписанию", "время", st.CheckTime)
			run, err := m.StartCheck()
			if err != nil {
				// Обычно это значит, что пользователь уже запустил проверку руками.
				m.log.Info("плановая проверка пропущена", "причина", err)
				continue
			}
			// Ждём завершения, чтобы следующая минута не запустила вторую.
			for range run.Events {
			}
		}
	}
}

// dueNow решает, пора ли запускать плановую проверку. Совпадение по
// минуте вместо точного момента: таймер может сместиться, а пропускать
// сутки из-за этого не хочется.
func dueNow(st State, now, last time.Time) bool {
	if !last.IsZero() && now.Sub(last) < 2*time.Minute {
		return false
	}
	// Проверка нужна, если запланированный на сегодня момент уже наступил,
	// а последняя проверка была раньше него.
	scheduled := st.NextRun(now.Add(-24 * time.Hour))
	if scheduled.After(now) {
		return false
	}
	return st.LastCheck.Before(scheduled)
}
