package probe

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// latencyWorkers — сколько серверов проверяется одновременно. Больше
// на роутере смысла не имеет: упрёмся в процессор, а не в сеть.
const latencyWorkers = 4

// CheckAll проверяет все серверы подписки: сначала задержку до каждого,
// затем скорость у части из них. Промежуточные результаты отдаются через
// onResult, чтобы интерфейс показывал ход проверки.
//
// prev — результаты прошлой проверки по ключу сервера. Они нужны дважды:
// чтобы в первую очередь мерить скорость там, где её ещё не знаем, и чтобы
// не терять уже известные значения для остальных серверов.
func CheckAll(ctx context.Context, servers []subscription.Server, opt Options,
	prev map[string]Result, onResult func(Result)) []Result {

	now := func() string { return time.Now().Format(time.RFC3339) }

	results := make([]Result, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, latencyWorkers)
	var mu sync.Mutex

	for i, s := range servers {
		wg.Add(1)
		go func(i int, s subscription.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := Result{
				Name:    s.Name,
				Address: s.Address,
				Port:    s.Port,
				Key:     s.Key(),
				Checked: now(),
			}
			latency, err := Latency(ctx, s.Endpoint(), opt.LatencyTries, opt.DialTimeout)
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Latency = float64(latency.Microseconds()) / 1000
				// Пока скорость не измерена в этот раз, показываем прошлую:
				// иначе сервер выглядел бы худшим просто потому, что до него
				// не дошла очередь.
				if old, ok := prev[r.Key]; ok && old.Speed > 0 {
					r.Speed = old.Speed
					r.SpeedStale = true
				}
			}

			mu.Lock()
			results[i] = r
			mu.Unlock()
			if onResult != nil {
				onResult(r)
			}
		}(i, s)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return results
	}

	// Скорость меряем последовательно: каждая проверка поднимает свой xray,
	// и параллельные замеры мешали бы друг другу делить канал.
	for _, idx := range speedCandidates(results, opt.SpeedTopN) {
		if ctx.Err() != nil {
			break
		}
		speed, err := Speed(ctx, servers[idx], opt)
		mu.Lock()
		if err != nil {
			// Сервер отвечает — он остаётся кандидатом, просто без замера
			// скорости: сравним его с другими по задержке.
			results[idx].SpeedError = err.Error()
		} else {
			results[idx].Speed = speed
			results[idx].SpeedStale = false
			results[idx].SpeedError = ""
		}
		results[idx].Checked = now()
		r := results[idx]
		mu.Unlock()
		if onResult != nil {
			onResult(r)
		}
	}
	return results
}

// speedCandidates выбирает, у каких серверов мерить скорость.
//
// Сначала идут те, о чьей скорости ничего не известно, — от самых
// отзывчивых. Затем самые быстрые по прошлым замерам: их стоит перемерить,
// потому что именно из них выбирается рабочий сервер. Так за несколько
// суточных проверок охватываются все серверы подписки, даже когда задержки
// у них одинаковые и ранжировать по ним нечего.
func speedCandidates(results []Result, n int) []int {
	type candidate struct {
		idx     int
		latency float64
		speed   float64
		known   bool
	}

	var alive []candidate
	for i, r := range results {
		if !r.Alive() {
			continue
		}
		alive = append(alive, candidate{
			idx:     i,
			latency: r.Latency,
			speed:   r.Speed,
			known:   r.Speed > 0,
		})
	}

	sort.SliceStable(alive, func(i, j int) bool {
		a, b := alive[i], alive[j]
		if a.known != b.known {
			return !a.known // неизвестные — вперёд
		}
		if a.known {
			return a.speed > b.speed
		}
		return a.latency < b.latency
	})

	if n > 0 && len(alive) > n {
		alive = alive[:n]
	}
	idx := make([]int, 0, len(alive))
	for _, c := range alive {
		idx = append(idx, c.idx)
	}
	return idx
}
