package fulltunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore собирает хранилище поверх заглушки iptables, которая
// держит «таблицу» в обычном файле.
func newTestStore(t *testing.T, excluded string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules")
	// Правило Кваса на месте — по нему определяется интерфейс сети.
	write(t, rules, "-A PREROUTING -i br0 -m set --match-set KVAS_LIST dst -j KVAS_MARK\n")

	conf := filepath.Join(dir, "kvas.conf")
	write(t, conf, "APP_VERSION=1.1.9\nroute_excluded_ip="+excluded+"\n")

	s := New(filepath.Join(dir, "fulltunnel"), conf)
	s.IptablesBin = fakeIptables(t, dir, rules)
	return s, rules
}

func TestEnableAddsHookAndChain(t *testing.T) {
	s, rules := newTestStore(t, "")

	if err := s.Set(true); err != nil {
		t.Fatalf("режим не включился: %v", err)
	}

	got := read(t, rules)
	if !strings.Contains(got, "-A PREROUTING -i br0 -j KVASWEB_FULL") {
		t.Errorf("правило на интерфейсе не добавлено:\n%s", got)
	}
	if !strings.Contains(got, "-A KVASWEB_FULL -j KVAS_MARK") {
		t.Errorf("пометка трафика не добавлена:\n%s", got)
	}

	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !st.Applied || st.Iface != "br0" {
		t.Errorf("состояние прочитано неверно: %+v", st)
	}
}

// Устройства, которым назначено «мимо туннеля», не должны заворачиваться
// даже в режиме полного туннеля — иначе настройка противоречит сама себе.
func TestExcludedDevicesSkipped(t *testing.T) {
	s, rules := newTestStore(t, "192.168.31.50+192.168.31.60")

	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}

	got := read(t, rules)
	for _, ip := range []string{"192.168.31.50", "192.168.31.60"} {
		if !strings.Contains(got, "-A KVASWEB_FULL -s "+ip+" -j RETURN") {
			t.Errorf("устройство %s не исключено:\n%s", ip, got)
		}
	}
	// RETURN обязан стоять раньше пометки, иначе он бесполезен.
	if strings.Index(got, "-s 192.168.31.50 -j RETURN") > strings.Index(got, "-A KVASWEB_FULL -j KVAS_MARK") {
		t.Error("исключения оказались после пометки трафика")
	}
}

func TestRepeatedApplyKeepsSingleRule(t *testing.T) {
	s, rules := newTestStore(t, "")
	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(read(t, rules), "-A PREROUTING -i br0 -j KVASWEB_FULL"); n != 1 {
		t.Errorf("правило продублировано %d раз", n)
	}
}

func TestDisableRemovesEverything(t *testing.T) {
	s, rules := newTestStore(t, "192.168.31.50")
	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(false); err != nil {
		t.Fatalf("режим не выключился: %v", err)
	}

	got := read(t, rules)
	if strings.Contains(got, "KVASWEB_FULL") {
		t.Errorf("следы режима остались:\n%s", got)
	}
	if !strings.Contains(got, "--match-set KVAS_LIST") {
		t.Errorf("правило Кваса задето:\n%s", got)
	}
	st, _ := s.Status()
	if st.Enabled || st.Applied {
		t.Errorf("состояние не сброшено: %+v", st)
	}
}

// `kvas init` пересоздаёт таблицы, и режим из них пропадает — сторож
// обязан вернуть его на место.
func TestKeepAppliedRestoresRule(t *testing.T) {
	s, rules := newTestStore(t, "")
	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}
	write(t, rules, "-A PREROUTING -i br0 -m set --match-set KVAS_LIST dst -j KVAS_MARK\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.KeepApplied(ctx, 20*time.Millisecond, func(err error) { t.Log("сторож:", err) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(read(t, rules), "-A PREROUTING -i br0 -j KVASWEB_FULL") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("правило не восстановлено:\n%s", read(t, rules))
}

// Пока Квас не настроен, включать режим не на что — и об этом надо
// сказать словами, а не молча ничего не сделать.
func TestEnableWithoutKvasRules(t *testing.T) {
	s, rules := newTestStore(t, "")
	write(t, rules, "")

	err := s.Set(true)
	if err == nil {
		t.Fatal("режим включился без правил Кваса")
	}
	if !strings.Contains(err.Error(), "kvas setup") {
		t.Errorf("подсказка невнятная: %v", err)
	}
}

// --- вспомогательное ---

// fakeIptables изображает iptables: таблица лежит в файле, поддержаны
// те операции, которыми пользуется модуль.
func fakeIptables(t *testing.T, dir, rules string) string {
	t.Helper()
	bin := filepath.Join(dir, "iptables")
	script := `#!/bin/sh
RULES=` + rules + `
# отбрасываем "-t mangle"
shift 2
op=$1; shift
chain=$1; shift
case "$op" in
	-S)
		if [ "$chain" = "PREROUTING" ]; then
			grep -- "-A PREROUTING" "$RULES" 2>/dev/null
		else
			grep -- "$chain" "$RULES" 2>/dev/null
		fi
		exit 0 ;;
	-N)
		grep -qx -- "-N $chain" "$RULES" 2>/dev/null && exit 1
		echo "-N $chain" >> "$RULES"; exit 0 ;;
	-F)
		grep -v -- "-A $chain " "$RULES" > "$RULES.tmp" 2>/dev/null
		mv "$RULES.tmp" "$RULES"; exit 0 ;;
	-X)
		grep -vx -- "-N $chain" "$RULES" > "$RULES.tmp" 2>/dev/null
		mv "$RULES.tmp" "$RULES"; exit 0 ;;
	-A)
		echo "-A $chain $*" >> "$RULES"; exit 0 ;;
	-C)
		grep -qx -- "-A $chain $*" "$RULES" 2>/dev/null && exit 0 || exit 1 ;;
	-D)
		grep -vx -- "-A $chain $*" "$RULES" > "$RULES.tmp" 2>/dev/null
		mv "$RULES.tmp" "$RULES"; exit 0 ;;
esac
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Файл состояния — источник истины: если режим выключили, пока сервис
// не работал, оставшееся правило нужно снять, а не считать нормой.
func TestKeepAppliedRemovesRuleWhenDisabled(t *testing.T) {
	s, rules := newTestStore(t, "")
	if err := s.Set(true); err != nil {
		t.Fatal(err)
	}
	// Выключаем в обход применения — как будто правку сделали вручную.
	write(t, filepath.Join(filepath.Dir(rules), "fulltunnel"), "off\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.KeepApplied(ctx, 20*time.Millisecond, func(err error) { t.Log("сторож:", err) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(read(t, rules), "KVASWEB_FULL") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("правило осталось при выключенном режиме:\n%s", read(t, rules))
}
