// Package autovpn хранит подписку на серверы VPN, проверяет их по
// расписанию и переключает Квас на лучший.
package autovpn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/clrmsc/kvas-web/web/internal/probe"
	"github.com/clrmsc/kvas-web/web/internal/subscription"
)

// State — сохраняемое состояние подписки.
type State struct {
	URL       string `json:"url"`        // ссылка на подписку; секрет, наружу отдаётся укороченной
	Enabled   bool   `json:"enabled"`    // проверять по расписанию
	AutoApply bool   `json:"auto_apply"` // переключаться на лучший сервер самостоятельно
	CheckTime string `json:"check_time"` // время суточной проверки, «ЧЧ:ММ»
	SpeedTopN int    `json:"speed_top_n"`

	LastCheck  time.Time      `json:"last_check"`
	LastError  string         `json:"last_error,omitempty"`
	Results    []probe.Result `json:"results"`
	ActiveKey  string         `json:"active_key"`  // адрес:порт применённого сервера
	ActiveName string         `json:"active_name"` // его подпись из подписки
	AppliedAt  time.Time      `json:"applied_at"`
}

// DefaultState — состояние до первой настройки.
func DefaultState() State {
	return State{
		Enabled:   true,
		AutoApply: true,
		CheckTime: "04:30",
		SpeedTopN: 5,
		Results:   []probe.Result{},
	}
}

// Configured сообщает, задана ли подписка.
func (s State) Configured() bool { return strings.TrimSpace(s.URL) != "" }

// NextRun возвращает ближайший момент суточной проверки после указанного.
func (s State) NextRun(after time.Time) time.Time {
	h, m, err := parseHHMM(s.CheckTime)
	if err != nil {
		h, m = 4, 30
	}
	next := time.Date(after.Year(), after.Month(), after.Day(), h, m, 0, 0, after.Location())
	if !next.After(after) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// ValidateCheckTime проверяет строку расписания.
func ValidateCheckTime(v string) error {
	if _, _, err := parseHHMM(v); err != nil {
		return err
	}
	return nil
}

func parseHHMM(v string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("время указывается как ЧЧ:ММ, например 04:30")
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("время указывается как ЧЧ:ММ, например 04:30")
	}
	return h, m, nil
}

// MaskURL прячет токен подписки. Обёртка над subscription.MaskURL,
// чтобы вызовы читались там, где речь о состоянии.
func MaskURL(raw string) string { return subscription.MaskURL(raw) }

func loadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultState(), nil
		}
		return DefaultState(), err
	}
	st := DefaultState()
	if err := json.Unmarshal(data, &st); err != nil {
		return DefaultState(), fmt.Errorf("файл подписки повреждён: %w", err)
	}
	if st.Results == nil {
		st.Results = []probe.Result{}
	}
	if st.SpeedTopN <= 0 {
		st.SpeedTopN = 5
	}
	return st, nil
}

func saveState(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".subscription-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// Файл содержит ссылку с токеном подписки — читать его может только root.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
