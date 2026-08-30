package kvas

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Conf — файл вида KEY=value (kvas.conf). Пары читаются и переписываются
// с сохранением остальных строк, чтобы не потерять чужие настройки.
type Conf struct {
	Path string
}

// Get возвращает значение ключа или пустую строку.
func (c Conf) Get(key string) (string, error) {
	f, err := os.Open(c.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), nil
		}
	}
	return "", sc.Err()
}

// GetAll читает файл целиком в карту — дешевле, чем несколько вызовов Get.
func (c Conf) GetAll() (map[string]string, error) {
	res := make(map[string]string)
	f, err := os.Open(c.Path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			res[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return res, sc.Err()
}

// Set заменяет значение ключа (или дописывает его в конец файла).
func (c Conf) Set(key, value string) error {
	data, err := os.ReadFile(c.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			if replaced {
				continue // дубликаты ключа схлопываем
			}
			out = append(out, key+"="+value)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		// Перед добавлением убираем возможную пустую строку в конце.
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, key+"="+value)
	}
	body := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	return WriteFileAtomic(c.Path, []byte(body), 0o644)
}

// WriteFileAtomic пишет файл через временный и rename — при пропадании
// питания роутера файл останется либо старым, либо новым, но не обрезанным.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	// Каталог может отсутствовать: `kvas upgrade` удаляет /opt/etc/xray
	// вместе с конфигурацией туннеля.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kvasweb-*")
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
