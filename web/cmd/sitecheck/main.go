// Команда sitecheck отвечает на вопрос «через какие серверы подписки
// открывается этот сайт». Нужна, когда туннель работает, а конкретный
// сервис через него не идёт: так видно, виноват ли выбранный сервер.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/probe"
	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

func main() {
	target := "https://www.instagram.com/"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	limit := 8
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &limit)
	}

	data, err := os.ReadFile("/opt/etc/kvas-web/subscription.json")
	if err != nil {
		fmt.Println("не прочитано состояние подписки:", err)
		os.Exit(1)
	}
	var state struct {
		URL       string `json:"url"`
		ActiveKey string `json:"active_key"`
		Results   []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		fmt.Println("состояние подписки не разобрано:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	servers, err := subscription.FetchAndParse(ctx, state.URL)
	if err != nil {
		fmt.Println("подписка не скачана:", err)
		os.Exit(1)
	}

	// Порядок берём из последней проверки: сначала те, что считаются лучшими.
	order := make(map[string]int, len(state.Results))
	for i, r := range state.Results {
		order[r.Key] = i
	}
	sorted := make([]subscription.Server, 0, len(servers))
	for pos := 0; pos < len(state.Results); pos++ {
		for _, s := range servers {
			if order[s.Key()] == pos {
				sorted = append(sorted, s)
			}
		}
	}
	for _, s := range servers {
		if _, ok := order[s.Key()]; !ok {
			sorted = append(sorted, s)
		}
	}
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}

	opt := probe.DefaultOptions()
	opt.XrayBin = "/opt/sbin/xray"
	opt.TunnelURL = target

	fmt.Printf("Проверяем %s через %d серверов подписки\n\n", target, len(sorted))
	for _, s := range sorted {
		mark := " "
		if s.Key() == state.ActiveKey {
			mark = "*"
		}
		latency, err := probe.TunnelCheck(ctx, s, opt)
		if err != nil {
			fmt.Printf("%s %-34s не открывается\n", mark, s.Name)
			continue
		}
		fmt.Printf("%s %-34s открывается, %d мс\n", mark, s.Name, latency.Milliseconds())
	}
	fmt.Println("\n* — сервер, который используется сейчас")
}
