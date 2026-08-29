// Package networks хранит подсети, которые нужно заворачивать в туннель
// независимо от DNS.
//
// Обычный путь Кваса такой: клиент спрашивает адрес домена, dnsmasq отдаёт
// ответ и заодно кладёт адрес в ipset. Но часть программ ходит к своим
// серверам по зашитым адресам, вообще не спрашивая DNS, — так делает
// Telegram, и его трафик идёт мимо туннеля. Для таких случаев подсети
// добавляются в таблицу вручную.
//
// Таблица ipset живёт в памяти и пересоздаётся при `kvas init`, поэтому
// список хранится на диске и периодически применяется заново.
package networks

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

// Store — список подсетей на диске и его применение к таблице ipset.
type Store struct {
	Path      string // файл со списком
	IpsetBin  string // /opt/sbin/ipset
	TableName string // KVAS_LIST

	mu sync.Mutex
}

// New создаёт хранилище с обычными для Кваса путями.
func New(path string) *Store {
	return &Store{
		Path:      path,
		IpsetBin:  kvas.FindFile("/opt/sbin/ipset", "/usr/sbin/ipset", "/sbin/ipset"),
		TableName: "KVAS_LIST",
	}
}

// List возвращает сохранённые подсети.
func (s *Store) List() ([]string, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if out == nil {
		out = []string{}
	}
	sort.Strings(out)
	return out, sc.Err()
}

// Add добавляет подсети в список и сразу применяет их. Возвращает,
// сколько записей оказалось новыми.
func (s *Store) Add(items []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.List()
	if err != nil {
		return 0, err
	}
	have := make(map[string]bool, len(existing))
	for _, e := range existing {
		have[e] = true
	}

	added := 0
	for _, raw := range items {
		net, err := normalize(raw)
		if err != nil {
			return added, err
		}
		if have[net] {
			continue
		}
		have[net] = true
		existing = append(existing, net)
		added++
	}
	if added == 0 {
		return 0, nil
	}

	sort.Strings(existing)
	if err := s.save(existing); err != nil {
		return 0, err
	}
	return added, s.applyLocked(existing)
}

// Remove убирает подсеть из списка и из таблицы.
func (s *Store) Remove(item string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	net, err := normalize(item)
	if err != nil {
		return err
	}
	existing, err := s.List()
	if err != nil {
		return err
	}

	kept := make([]string, 0, len(existing))
	found := false
	for _, e := range existing {
		if e == net {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil
	}
	if err := s.save(kept); err != nil {
		return err
	}
	// Из таблицы убираем сразу: иначе подсеть продолжит уходить в туннель
	// до следующего пересоздания таблицы.
	if s.IpsetBin != "" {
		_ = exec.Command(s.IpsetBin, "del", s.TableName, net).Run()
	}
	return nil
}

// Apply добавляет сохранённые подсети в таблицу. Вызывается при запуске и
// по расписанию: `kvas init` пересоздаёт таблицу и стирает наши записи.
func (s *Store) Apply() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, err := s.List()
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	return len(items), s.applyLocked(items)
}

func (s *Store) applyLocked(items []string) error {
	if s.IpsetBin == "" {
		return fmt.Errorf("не найден ipset")
	}
	for _, net := range items {
		// timeout 0 — запись без срока: у таблицы Кваса записи живут сутки,
		// а подсети должны оставаться, пока их не убрали из списка.
		out, err := exec.Command(s.IpsetBin, "add", s.TableName, net, "timeout", "0", "-exist").CombinedOutput()
		if err != nil {
			return fmt.Errorf("не удалось добавить %s: %s", net, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (s *Store) save(items []string) error {
	body := strings.Join(items, "\n")
	if body != "" {
		body += "\n"
	}
	return kvas.WriteFileAtomic(s.Path, []byte(body), 0o644)
}

// normalize проверяет подсеть или одиночный адрес и приводит к единому виду.
func normalize(raw string) (string, error) {
	return kvas.NormalizeIP(strings.TrimSpace(raw))
}

// KeepApplied периодически возвращает подсети в таблицу: её пересоздают
// и `kvas init`, и перезагрузка роутера.
func (s *Store) KeepApplied(ctx context.Context, every time.Duration, report func(int, error)) {
	n, err := s.Apply()
	if report != nil {
		report(n, err)
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.Apply(); err != nil && report != nil {
				report(n, err)
			}
		}
	}
}
