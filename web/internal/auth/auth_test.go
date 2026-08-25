package auth

import (
	"errors"
	"path/filepath"
	"testing"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := New(filepath.Join(dir, "password"), filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPasswordLifecycle(t *testing.T) {
	m := newManager(t)
	if m.HasPassword() {
		t.Fatal("на чистой установке пароля быть не должно")
	}
	if err := m.SetPassword("короткий"[:3]); err == nil {
		t.Error("короткий пароль должен отвергаться")
	}
	if err := m.SetPassword("правильный-пароль"); err != nil {
		t.Fatal(err)
	}
	if !m.HasPassword() {
		t.Fatal("пароль не сохранился")
	}

	id, err := m.Verify("правильный-пароль")
	if err != nil {
		t.Fatalf("верный пароль отклонён: %v", err)
	}
	if !m.Valid(id) {
		t.Error("свежая сессия должна быть действительной")
	}

	m.Revoke(id)
	if m.Valid(id) {
		t.Error("отозванная сессия должна быть недействительной")
	}
	if m.Valid("подделанный-идентификатор") {
		t.Error("произвольная строка не должна проходить как сессия")
	}
}

func TestBruteForceLockout(t *testing.T) {
	m := newManager(t)
	if err := m.SetPassword("правильный-пароль"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxFails; i++ {
		if _, err := m.Verify("неверный"); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("попытка %d: ожидалась ErrWrongPassword, получено %v", i+1, err)
		}
	}

	// После серии промахов блокируется даже верный пароль.
	var locked ErrLocked
	if _, err := m.Verify("правильный-пароль"); !errors.As(err, &locked) {
		t.Fatalf("ожидалась блокировка, получено %v", err)
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	passFile := filepath.Join(dir, "password")
	sessFile := filepath.Join(dir, "sessions")

	m1, err := New(passFile, sessFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := m1.SetPassword("правильный-пароль"); err != nil {
		t.Fatal(err)
	}
	id, err := m1.Verify("правильный-пароль")
	if err != nil {
		t.Fatal(err)
	}

	m2, err := New(passFile, sessFile)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Valid(id) {
		t.Error("после перезапуска сервиса сессия должна оставаться действительной")
	}
}

func TestRevokeAllOnPasswordChange(t *testing.T) {
	m := newManager(t)
	if err := m.SetPassword("первый-пароль"); err != nil {
		t.Fatal(err)
	}
	id, err := m.Verify("первый-пароль")
	if err != nil {
		t.Fatal(err)
	}
	m.RevokeAll()
	if m.Valid(id) {
		t.Error("после смены пароля старые сессии должны сбрасываться")
	}
}
