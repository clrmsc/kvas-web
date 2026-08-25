// Package auth отвечает за пароль администратора, сессии и защиту от подбора.
//
// Пароль хранится как PBKDF2-SHA256 со случайной солью, а не как голый md5,
// как это было в shell-версии веб-морды. Идентификатор сессии живёт в
// HttpOnly-cookie и никогда не попадает в URL.
package auth

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Итераций достаточно, чтобы подбор был дорогим, но вход на MIPS-роутере
	// оставался в пределах десятых долей секунды.
	iterations = 64000
	saltLen    = 16
	keyLen     = 32

	// SessionTTL — сколько живёт сессия без активности.
	SessionTTL = 12 * time.Hour

	maxFails    = 5
	lockoutTime = 5 * time.Minute

	// MinPasswordLen — минимальная длина пароля администратора.
	MinPasswordLen = 8
)

// ErrLocked возвращается, когда вход временно заблокирован после серии
// неудачных попыток.
type ErrLocked struct{ Retry time.Duration }

func (e ErrLocked) Error() string {
	return fmt.Sprintf("вход заблокирован, попробуйте через %d с", int(e.Retry.Seconds()))
}

var (
	// ErrNoPassword — пароль ещё не задан, нужен первичный setup.
	ErrNoPassword = errors.New("пароль не задан")
	// ErrWrongPassword — неверный пароль.
	ErrWrongPassword = errors.New("неверный пароль")
	// ErrPasswordTooShort — пароль короче MinPasswordLen.
	ErrPasswordTooShort = fmt.Errorf("пароль короче %d символов", MinPasswordLen)
)

type session struct {
	seen time.Time
}

// Manager хранит состояние аутентификации. Все методы безопасны для
// параллельного вызова.
type Manager struct {
	passFile    string
	sessionFile string

	mu       sync.Mutex
	sessions map[string]*session
	fails    int
	lastFail time.Time
}

// New создаёт менеджер и подхватывает ранее сохранённые сессии.
func New(passFile, sessionFile string) (*Manager, error) {
	m := &Manager{
		passFile:    passFile,
		sessionFile: sessionFile,
		sessions:    make(map[string]*session),
	}
	if err := os.MkdirAll(filepath.Dir(passFile), 0o700); err != nil {
		return nil, err
	}
	m.loadSessions()
	return m, nil
}

// HasPassword сообщает, прошла ли первичная настройка.
func (m *Manager) HasPassword() bool {
	st, err := os.Stat(m.passFile)
	return err == nil && st.Size() > 0
}

// SetPassword задаёт или меняет пароль администратора.
func (m *Manager) SetPassword(pass string) error {
	if len([]rune(pass)) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key, err := pbkdf2.Key(sha256.New, pass, salt, iterations, keyLen)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("pbkdf2-sha256$%d$%s$%s\n", iterations, hex.EncodeToString(salt), hex.EncodeToString(key))
	return writeFileAtomic(m.passFile, []byte(line), 0o600)
}

// Verify проверяет пароль с учётом защиты от подбора и при успехе
// возвращает идентификатор новой сессии.
func (m *Manager) Verify(pass string) (string, error) {
	m.mu.Lock()
	if m.fails >= maxFails {
		if since := time.Since(m.lastFail); since < lockoutTime {
			m.mu.Unlock()
			return "", ErrLocked{Retry: lockoutTime - since}
		}
		m.fails = 0
	}
	m.mu.Unlock()

	stored, err := os.ReadFile(m.passFile)
	if err != nil {
		return "", ErrNoPassword
	}
	ok, err := checkHash(strings.TrimSpace(string(stored)), pass)
	if err != nil {
		return "", err
	}
	if !ok {
		m.mu.Lock()
		m.fails++
		m.lastFail = time.Now()
		m.mu.Unlock()
		return "", ErrWrongPassword
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails = 0
	id := rand.Text()
	m.sessions[id] = &session{seen: time.Now()}
	m.persistLocked()
	return id, nil
}

// Valid проверяет сессию и продлевает её при успехе.
func (m *Manager) Valid(id string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false
	}
	if time.Since(s.seen) > SessionTTL {
		delete(m.sessions, id)
		m.persistLocked()
		return false
	}
	s.seen = time.Now()
	return true
}

// Revoke завершает одну сессию.
func (m *Manager) Revoke(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	m.persistLocked()
}

// RevokeAll завершает все сессии — вызывается при смене пароля.
func (m *Manager) RevokeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = make(map[string]*session)
	m.persistLocked()
}

func checkHash(stored, pass string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, errors.New("файл пароля повреждён, задайте пароль заново")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, errors.New("файл пароля повреждён, задайте пароль заново")
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false, errors.New("файл пароля повреждён, задайте пароль заново")
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false, errors.New("файл пароля повреждён, задайте пароль заново")
	}
	got, err := pbkdf2.Key(sha256.New, pass, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// persistLocked сохраняет сессии на диск. Вызывается под удержанным mu.
func (m *Manager) persistLocked() {
	if m.sessionFile == "" {
		return
	}
	var b strings.Builder
	for id, s := range m.sessions {
		fmt.Fprintf(&b, "%s %d\n", id, s.seen.Unix())
	}
	_ = writeFileAtomic(m.sessionFile, []byte(b.String()), 0o600)
}

func (m *Manager) loadSessions() {
	f, err := os.Open(m.sessionFile)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id, ts, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		seen := time.Unix(sec, 0)
		if time.Since(seen) > SessionTTL {
			continue
		}
		m.sessions[id] = &session{seen: seen}
	}
}

// writeFileAtomic пишет через временный файл и переименование, чтобы
// внезапная перезагрузка роутера не оставила обрезанный файл пароля.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
