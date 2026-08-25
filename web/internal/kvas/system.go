package kvas

import (
	"bufio"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcessRunning ищет процесс с указанным именем среди /proc/<pid>/comm.
// Это дешевле, чем звать pidof внешним процессом на каждый опрос статуса.
func ProcessRunning(name string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == name {
			return true
		}
	}
	return false
}

// PortListening проверяет, слушает ли кто-то указанный TCP-порт на локальной
// машине, разбирая /proc/net/tcp — на роутере может не быть ни ss, ни netstat.
func PortListening(port int) bool {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		found := scanProcNetTCP(f, port)
		f.Close()
		if found {
			return true
		}
	}
	return false
}

func scanProcNetTCP(f *os.File, port int) bool {
	const stateListen = "0A"
	sc := bufio.NewScanner(f)
	sc.Scan() // заголовок
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != stateListen {
			continue
		}
		_, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		raw, err := hex.DecodeString(portHex)
		if err != nil || len(raw) != 2 {
			continue
		}
		if int(raw[0])<<8|int(raw[1]) == port {
			return true
		}
	}
	return false
}

// PackageVersion достаёт версию установленного пакета из базы opkg.
func PackageVersion(pkg string) string {
	f, err := os.Open("/opt/lib/opkg/status")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	inPkg := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Package: "):
			inPkg = strings.TrimSpace(strings.TrimPrefix(line, "Package: ")) == pkg
		case inPkg && strings.HasPrefix(line, "Version: "):
			return strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
		}
	}
	return ""
}

// FileHasLine сообщает, есть ли в файле строка, содержащая подстроку.
// Используется для проверки включённых директив dnsmasq.
func FileHasLine(path, substr string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// RemoveLines убирает из файла все строки, содержащие подстроку.
func RemoveLines(path, substr string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, substr) {
			continue
		}
		out = append(out, line)
	}
	body := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	return WriteFileAtomic(path, []byte(body), 0o644)
}

// AppendLine дописывает строку в конец файла, если её там ещё нет.
func AppendLine(path, line string) error {
	if FileHasLine(path, line) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	body := strings.TrimRight(string(data), "\n")
	if body != "" {
		body += "\n"
	}
	body += line + "\n"
	return WriteFileAtomic(path, []byte(body), 0o644)
}
