package autovpn

import (
	"testing"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/probe"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestUpdateHistoryKeepsLastDays(t *testing.T) {
	var history map[string][]HistoryPoint

	// Семь дней подряд, а храним пять последних.
	for i := 1; i <= 7; i++ {
		results := []probe.Result{{
			Key: "a:1", Latency: 3, Tunnel: float64(200 + i), Speed: float64(100 + i),
		}}
		history = updateHistory(history, results, day("2026-08-0"+string(rune('0'+i))))
	}

	points := history["a:1"]
	if len(points) != historyDays {
		t.Fatalf("сохранено %d дней, ожидалось %d", len(points), historyDays)
	}
	if points[0].Date != "2026-08-03" || points[4].Date != "2026-08-07" {
		t.Errorf("окно дней сдвинуто: %s…%s", points[0].Date, points[4].Date)
	}
}

func TestUpdateHistoryOverwritesSameDay(t *testing.T) {
	now := day("2026-08-26")
	history := updateHistory(nil, []probe.Result{
		{Key: "a:1", Latency: 3, Tunnel: 300, Speed: 120},
	}, now)

	// Повторная проверка в тот же день: запись обновляется, а не добавляется.
	history = updateHistory(history, []probe.Result{
		{Key: "a:1", Latency: 3, Tunnel: 280},
	}, now)

	points := history["a:1"]
	if len(points) != 1 {
		t.Fatalf("за один день должно быть одно значение, получено %d", len(points))
	}
	if points[0].Tunnel != 280 {
		t.Errorf("задержка должна обновиться, получено %v", points[0].Tunnel)
	}
	// Скорость в этот раз не мерили — прежнее значение за день сохраняется.
	if points[0].Speed != 120 {
		t.Errorf("скорость за день не должна теряться, получено %v", points[0].Speed)
	}
}

func TestUpdateHistorySkipsUnusableResults(t *testing.T) {
	now := day("2026-08-26")
	history := updateHistory(nil, []probe.Result{
		{Key: "мёртвый", Error: "нет связи"},
		{Key: "без туннеля", Latency: 3, TunnelError: "нет доступа"},
		{Key: "не мерян", Latency: 3},
		{Key: "рабочий", Latency: 3, Tunnel: 250, Speed: 90},
	}, now)

	if len(history) != 1 {
		t.Fatalf("в историю попало %d серверов, ожидался один: %v", len(history), history)
	}
	if _, ok := history["рабочий"]; !ok {
		t.Error("рабочий сервер должен попасть в историю")
	}
}

func TestUpdateHistoryForgetsRemovedServers(t *testing.T) {
	now := day("2026-08-26")
	history := updateHistory(nil, []probe.Result{
		{Key: "старый", Latency: 3, Tunnel: 300, Speed: 100},
	}, now)

	// Сервер пропал из подписки — держать его историю незачем.
	history = updateHistory(history, []probe.Result{
		{Key: "новый", Latency: 3, Tunnel: 280, Speed: 110},
	}, now.AddDate(0, 0, 1))

	if _, ok := history["старый"]; ok {
		t.Error("история пропавшего сервера должна удаляться")
	}
	if _, ok := history["новый"]; !ok {
		t.Error("история нового сервера должна сохраниться")
	}
}
