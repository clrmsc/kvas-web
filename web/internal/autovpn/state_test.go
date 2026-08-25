package autovpn

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	st := DefaultState()
	st.CheckTime = "04:30"

	// До назначенного времени проверка планируется на сегодня.
	morning := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	if got := st.NextRun(morning); got.Day() != 25 || got.Hour() != 4 || got.Minute() != 30 {
		t.Errorf("до срока получили %s", got)
	}

	// После — на следующие сутки.
	evening := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)
	if got := st.NextRun(evening); got.Day() != 26 || got.Hour() != 4 {
		t.Errorf("после срока получили %s", got)
	}
}

func TestValidateCheckTime(t *testing.T) {
	for _, ok := range []string{"00:00", "4:05", "23:59", " 12:30 "} {
		if err := ValidateCheckTime(ok); err != nil {
			t.Errorf("время %q отвергнуто: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "24:00", "12:60", "полдень", "12", "12:30:45"} {
		if err := ValidateCheckTime(bad); err == nil {
			t.Errorf("время %q должно отвергаться", bad)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscription.json")
	st := DefaultState()
	st.URL = "https://example.io/subscription/vless/token"
	st.CheckTime = "03:15"
	st.ActiveName = "🇳🇱 Netherlands"

	if err := saveState(path, st); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.URL != st.URL || loaded.CheckTime != "03:15" || loaded.ActiveName != st.ActiveName {
		t.Errorf("состояние прочитано неверно: %+v", loaded)
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "нет.json"))
	if err != nil {
		t.Fatalf("отсутствующий файл должен давать состояние по умолчанию: %v", err)
	}
	if st.CheckTime != "04:30" || !st.Enabled {
		t.Errorf("умолчания не применились: %+v", st)
	}
}

func TestDueNow(t *testing.T) {
	st := DefaultState()
	st.CheckTime = "04:30"
	now := time.Date(2026, 8, 25, 4, 30, 30, 0, time.UTC)

	// Проверок ещё не было — пора.
	if !dueNow(st, now, time.Time{}) {
		t.Error("первая проверка должна запуститься по расписанию")
	}

	// Проверка сегодня уже прошла — повторно не запускаем.
	st.LastCheck = time.Date(2026, 8, 25, 4, 30, 5, 0, time.UTC)
	if dueNow(st, now.Add(time.Minute*3), time.Time{}) {
		t.Error("повторная проверка в те же сутки не нужна")
	}

	// Сутки спустя — снова пора.
	tomorrow := now.AddDate(0, 0, 1)
	if !dueNow(st, tomorrow, time.Time{}) {
		t.Error("на следующие сутки проверка должна повториться")
	}

	// До назначенного времени не запускаем.
	early := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	if dueNow(st, early, time.Time{}) {
		t.Error("до назначенного времени запускать нельзя")
	}
}
