package probe

import (
	"context"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// latencyWorkers — сколько серверов проверяется одновременно. Больше
// на роутере смысла не имеет: упрёмся в процессор, а не в сеть.
const latencyWorkers = 4

// CheckAll проверяет все серверы подписки: сначала задержку до каждого,
// затем скорость у лучших по задержке. Промежуточные результаты
// отдаются через onResult, чтобы интерфейс показывал ход проверки.
func CheckAll(ctx context.Context, servers []subscription.Server, opt Options, onResult func(Result)) []Result {
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
	for _, idx := range topAlive(results, opt.SpeedTopN) {
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

// topAlive возвращает индексы n самых отзывчивых доступных серверов.
func topAlive(results []Result, n int) []int {
	type pair struct {
		idx     int
		latency float64
	}
	var alive []pair
	for i, r := range results {
		if r.Alive() {
			alive = append(alive, pair{i, r.Latency})
		}
	}
	for i := 1; i < len(alive); i++ {
		for j := i; j > 0 && alive[j].latency < alive[j-1].latency; j-- {
			alive[j], alive[j-1] = alive[j-1], alive[j]
		}
	}
	if n > 0 && len(alive) > n {
		alive = alive[:n]
	}
	idx := make([]int, 0, len(alive))
	for _, p := range alive {
		idx = append(idx, p.idx)
	}
	return idx
}
