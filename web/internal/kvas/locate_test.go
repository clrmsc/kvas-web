package kvas

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindFile(t *testing.T) {
	dir := t.TempDir()
	second := filepath.Join(dir, "второй")
	if err := os.WriteFile(second, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Возвращается первый существующий, отсутствующие пропускаются.
	got := FindFile(filepath.Join(dir, "нет"), second, filepath.Join(dir, "тоже нет"))
	if got != second {
		t.Errorf("получено %q, ожидалось %q", got, second)
	}

	if FindFile(filepath.Join(dir, "нет"), "") != "" {
		t.Error("когда ничего не найдено, должна возвращаться пустая строка")
	}
	if FindFile() != "" {
		t.Error("пустой список должен давать пустую строку")
	}
}
