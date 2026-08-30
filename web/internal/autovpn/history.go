package autovpn

import (
	"sort"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/probe"
)

// historyDays — сколько дней показывать в таблице. Больше на экране роутера
// не помещается, а меньше не даёт понять, стабилен ли сервер.
const historyDays = 5

// HistoryPoint — итог одного дня по одному серверу.
type HistoryPoint struct {
	Date   string  `json:"date"`       // ГГГГ-ММ-ДД
	Tunnel float64 `json:"tunnel_ms"`  // задержка через туннель
	Speed  float64 `json:"speed_mbps"` // 0, если в этот день скорость не мерили
}

// updateHistory дописывает итоги проверки в историю и оставляет последние
// historyDays дней.
//
// За сутки проверок может быть несколько (плановая и запущенные вручную),
// поэтому запись за день перезаписывается последним измерением. Скорость
// меряется не у всех серверов, и уже известное за этот день значение не
// затирается нулём — иначе колонка выглядела бы пустой.
func updateHistory(history map[string][]HistoryPoint, results []probe.Result, now time.Time) map[string][]HistoryPoint {
	if history == nil {
		history = make(map[string][]HistoryPoint, len(results))
	}
	today := now.Format("2006-01-02")

	for _, r := range results {
		if !r.Alive() || r.Tunnel <= 0 {
			// Недоступный сервер в историю не пишем: пустая колонка сама
			// говорит, что в этот день он не отвечал.
			continue
		}

		point := HistoryPoint{Date: today, Tunnel: r.Tunnel, Speed: r.Speed}
		points := history[r.Key]

		replaced := false
		for i := range points {
			if points[i].Date != today {
				continue
			}
			if point.Speed == 0 {
				point.Speed = points[i].Speed
			}
			points[i] = point
			replaced = true
			break
		}
		if !replaced {
			points = append(points, point)
		}

		sort.Slice(points, func(i, j int) bool { return points[i].Date < points[j].Date })
		if len(points) > historyDays {
			points = points[len(points)-historyDays:]
		}
		history[r.Key] = points
	}

	// Серверы, пропавшие из подписки, в истории не держим.
	present := make(map[string]bool, len(results))
	for _, r := range results {
		present[r.Key] = true
	}
	for key := range history {
		if !present[key] {
			delete(history, key)
		}
	}
	return history
}
