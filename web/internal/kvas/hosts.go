package kvas

import (
	"bufio"
	"os"
	"sort"
	"strings"
)

// ReadList читает список строк (домены, правила) из файла Кваса,
// пропуская пустые строки и комментарии.
func ReadList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
	return out, sc.Err()
}

// Tag — закваска: именованная группа доменов из tags.list.
type Tag struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
}

// ReadTags разбирает tags.list — ini-подобный файл, где секция [имя]
// открывает список доменов.
func ReadTags(path string) ([]Tag, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Tag{}, nil
		}
		return nil, err
	}
	defer f.Close()

	tags := []Tag{}
	var cur *Tag
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			tags = append(tags, Tag{
				Name:    strings.TrimSpace(line[1 : len(line)-1]),
				Domains: []string{},
			})
			cur = &tags[len(tags)-1]
			continue
		}
		if cur != nil {
			cur.Domains = append(cur.Domains, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
	return tags, nil
}

// InSet превращает список в множество для быстрой проверки вхождения.
func InSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, it := range items {
		set[it] = true
	}
	return set
}
