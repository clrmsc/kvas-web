// Package fulltunnel заворачивает в туннель весь трафик локальной сети,
// а не только адреса из списка Кваса.
//
// Квас помечает для туннеля лишь то, что попало в его таблицу ipset.
// Здесь добавляется правило, помечающее вообще весь трафик из локальной
// сети; всё остальное — исключения для локальных адресов, DNS и ICMP —
// уже сделано в цепочке KVAS_MARK, её и переиспользуем.
//
// Устройства, которым в Квасе назначено «мимо туннеля», обходятся
// стороной: для них в собственной цепочке стоит RETURN до пометки.
package fulltunnel

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/kvas"
)

const (
	// chain — собственная цепочка: в ней исключения по источнику и переход
	// к пометке. Отдельная цепочка нужна, чтобы правила Кваса остались
	// нетронутыми, а наши всегда можно было снять целиком.
	chain = "KVASWEB_FULL"
	// markChain — цепочка Кваса, которая ставит метку туннеля.
	markChain = "KVAS_MARK"
	table     = "mangle"
)

// Store включает и выключает режим и следит, чтобы правило не потерялось.
type Store struct {
	StateFile   string // файл с выбором пользователя
	KvasConf    string // откуда читаются устройства «мимо туннеля»
	IptablesBin string

	mu sync.Mutex
}

// New создаёт хранилище с путями по умолчанию.
func New(stateFile, kvasConf string) *Store {
	return &Store{
		StateFile:   stateFile,
		KvasConf:    kvasConf,
		IptablesBin: kvas.FindFile("/opt/sbin/iptables", "/usr/sbin/iptables", "/sbin/iptables"),
	}
}

// Status — состояние режима для интерфейса.
type Status struct {
	Enabled  bool     `json:"enabled"`  // что выбрал пользователь
	Applied  bool     `json:"applied"`  // стоит ли правило прямо сейчас
	Iface    string   `json:"iface"`    // интерфейс локальной сети
	Excluded []string `json:"excluded"` // устройства, идущие мимо туннеля
}

// Status возвращает и выбор пользователя, и действительное положение дел:
// расхождение между ними означает, что правила кто-то пересоздал.
func (s *Store) Status() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{Enabled: s.enabled(), Excluded: s.excluded()}
	iface, err := s.lanIface()
	if err != nil {
		return st, err
	}
	st.Iface = iface
	st.Applied = s.hookPresent(iface)
	return st, nil
}

// Set включает или выключает режим и сразу применяет его.
func (s *Store) Set(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if on {
		if err := s.applyLocked(); err != nil {
			return err
		}
	} else if err := s.removeLocked(); err != nil {
		return err
	}
	return s.save(on)
}

// KeepApplied возвращает правило на место: таблицы пересоздаются при
// `kvas init` и после перезагрузки роутера, и наш режим из них пропадает.
func (s *Store) KeepApplied(ctx context.Context, every time.Duration, report func(error)) {
	// Первый заход сразу: после перезагрузки роутера режим должен
	// восстановиться вместе с сервисом, а не через десять минут.
	if err := s.restore(); err != nil && report != nil {
		report(err)
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.restore(); err != nil && report != nil {
				report(err)
			}
		}
	}
}

// restore приводит правила к тому, что выбрал пользователь: файл
// состояния — источник истины. Так режим не только возвращается после
// `kvas init`, но и снимается, если его выключили, пока сервис не работал.
func (s *Store) restore() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled() {
		return s.applyLocked()
	}
	iface, err := s.lanIface()
	if err != nil || !s.hookPresent(iface) {
		return nil
	}
	return s.removeLocked()
}

// applyLocked пересобирает цепочку целиком: так режим переживает и смену
// списка устройств «мимо туннеля», и постороннее вмешательство в таблицы.
func (s *Store) applyLocked() error {
	if s.IptablesBin == "" {
		return fmt.Errorf("не найден iptables")
	}
	iface, err := s.lanIface()
	if err != nil {
		return err
	}

	want := s.wantedRules()
	if s.hookPresent(iface) && s.rulesMatch(want) {
		return nil
	}

	// Пересоздаём цепочку: -N на существующей вернёт ошибку, поэтому
	// сначала пробуем создать, а затем всё равно очищаем.
	s.run("-N", chain)
	if out, err := s.run("-F", chain); err != nil {
		return fmt.Errorf("не удалось очистить цепочку %s: %s", chain, out)
	}
	for _, args := range want {
		if out, err := s.run(append([]string{"-A", chain}, args...)...); err != nil {
			return fmt.Errorf("правило не добавлено: %s", out)
		}
	}
	if !s.hookPresent(iface) {
		if out, err := s.run("-A", "PREROUTING", "-i", iface, "-j", chain); err != nil {
			return fmt.Errorf("не удалось включить режим на интерфейсе %s: %s", iface, out)
		}
	}
	return nil
}

func (s *Store) removeLocked() error {
	if s.IptablesBin == "" {
		return fmt.Errorf("не найден iptables")
	}
	iface, err := s.lanIface()
	if err != nil {
		// Правил Кваса нет — снимать нечего.
		return s.dropChain()
	}
	// Дубли правила возможны, если таблицы пересоздавались на ходу.
	for i := 0; i < 8 && s.hookPresent(iface); i++ {
		if out, err := s.run("-D", "PREROUTING", "-i", iface, "-j", chain); err != nil {
			return fmt.Errorf("не удалось выключить режим: %s", out)
		}
	}
	return s.dropChain()
}

func (s *Store) dropChain() error {
	s.run("-F", chain)
	s.run("-X", chain)
	return nil
}

// wantedRules — содержимое цепочки: сначала пропускаем устройства,
// которым назначено «мимо туннеля», затем помечаем всё остальное.
func (s *Store) wantedRules() [][]string {
	rules := make([][]string, 0, 4)
	for _, ip := range s.excluded() {
		rules = append(rules, []string{"-s", ip, "-j", "RETURN"})
	}
	return append(rules, []string{"-j", markChain})
}

func (s *Store) rulesMatch(want [][]string) bool {
	out, err := s.run("-S", chain)
	if err != nil {
		return false
	}
	var have []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "-A "+chain+" ") {
			continue
		}
		have = append(have, strings.TrimPrefix(line, "-A "+chain+" "))
	}
	if len(have) != len(want) {
		return false
	}
	for i := range want {
		if have[i] != strings.Join(want[i], " ") {
			return false
		}
	}
	return true
}

// lanIface определяет интерфейс локальной сети по правилу Кваса: жёстко
// зашивать br0 нельзя, на других роутерах сеть называется иначе.
func (s *Store) lanIface() (string, error) {
	out, err := s.run("-S", "PREROUTING")
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать правила: %s", out)
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "-j "+markChain) {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-i" && i+1 < len(fields) {
				return fields[i+1], nil
			}
		}
	}
	return "", fmt.Errorf("Квас ещё не настроил маркировку трафика — выполните kvas setup")
}

func (s *Store) hookPresent(iface string) bool {
	_, err := s.run("-C", "PREROUTING", "-i", iface, "-j", chain)
	return err == nil
}

// excluded возвращает адреса устройств, которым в Квасе назначено
// «мимо туннеля». В kvas.conf они записаны через «+».
func (s *Store) excluded() []string {
	if s.KvasConf == "" {
		return nil
	}
	raw, err := kvas.Conf{Path: s.KvasConf}.Get("route_excluded_ip")
	if err != nil {
		return nil
	}
	var out []string
	for _, ip := range strings.Split(raw, "+") {
		ip = strings.TrimSpace(ip)
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

func (s *Store) run(args ...string) (string, error) {
	full := append([]string{"-t", table}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.IptablesBin, full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Store) enabled() bool {
	data, err := os.ReadFile(s.StateFile)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "on"
}

func (s *Store) save(on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	if err := os.MkdirAll(filepath.Dir(s.StateFile), 0o700); err != nil {
		return err
	}
	return kvas.WriteFileAtomic(s.StateFile, []byte(value+"\n"), 0o644)
}
