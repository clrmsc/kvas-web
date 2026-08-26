package probe

import (
	"context"
	"errors"
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

	// Второй этап: у каждого откликнувшегося сервера проверяем, ходит ли
	// через него трафик наружу, и меряем задержку такого запроса. Открытый
	// порт входного узла об этом ничего не говорит, а задержка до самого
	// узла у всех серверов одинаковая, если точки входа стоят рядом.
	probeBroken := false
	for i := range results {
		if ctx.Err() != nil {
			break
		}
		if !results[i].Reachable() {
			continue
		}
		tunnel, err := TunnelCheck(ctx, servers[i], opt)
		if errors.Is(err, ErrProbeUnavailable) {
			// Проверять нечем: xray не нашёлся или не стартует. Оставляем
			// результаты как есть — иначе объявим нерабочими все серверы.
			probeBroken = true
			break
		}
		mu.Lock()
		if err != nil {
			results[i].TunnelError = err.Error()
			results[i].Tunnel = 0
			// Прошлая скорость относится к работавшему тогда туннелю —
			// сейчас сервер наружу не пускает, показывать её нельзя.
			results[i].Speed = 0
			results[i].SpeedStale = false
		} else {
			results[i].Tunnel = float64(tunnel.Microseconds()) / 1000
			results[i].TunnelError = ""
		}
		results[i].Checked = now()
		r := results[i]
		mu.Unlock()
		if onResult != nil {
			onResult(r)
		}
	}

	if err := ctx.Err(); err != nil || probeBroken {
		return results
	}

	// Третий этап: скорость. Меряем последовательно — каждая проверка
	// поднимает свой xray, и параллельные замеры делили бы канал.
	for _, idx := range speedCandidates(results, opt.SpeedTopN, opt.ActiveKey) {
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
// Первым идёт текущий рабочий сервер — его данные должны быть свежими,
// чтобы сравнение с соперниками было честным. Затем те, о чьей скорости
// ничего не известно, — от самых отзывчивых. Затем самые быстрые по прошлым
// замерам: именно из них выбирается рабочий сервер. Так за несколько
// суточных проверок охватываются все серверы подписки, даже когда задержки
// у них одинаковые и ранжировать по ним нечего.
func speedCandidates(results []Result, n int, activeKey string) []int {
	type candidate struct {
		idx     int
		latency float64
		speed   float64
		known   bool
		active  bool
	}

	var alive []candidate
	for i, r := range results {
		if !r.Alive() {
			continue
		}
		// Сравниваем по задержке через туннель, если она известна: отклик
		// входного узла у всех серверов одинаков и ничего не различает.
		latency := r.Tunnel
		if latency == 0 {
			latency = r.Latency
		}
		alive = append(alive, candidate{
			idx:     i,
			latency: latency,
			speed:   r.Speed,
			known:   r.Speed > 0,
			active:  activeKey != "" && r.Key == activeKey,
		})
	}

	sort.SliceStable(alive, func(i, j int) bool {
		a, b := alive[i], alive[j]
		if a.active != b.active {
			return a.active // текущий сервер — первым
		}
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
