package kvas

import "os"

// FindFile возвращает первый существующий путь из списка или пустую строку.
// Нужен там, где Квас раскладывает файлы по-разному в зависимости от того,
// прошла ли уже первичная настройка.
func FindFile(candidates ...string) string {
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// Кандидаты для xray. Квас ставит бинарник через opkg в /opt/sbin,
// а init-скрипт кладёт в свой каталог и при настройке делает на него
// ссылку S24xray — управляет он именно ссылкой.
var (
	XrayBinCandidates = []string{
		"/opt/sbin/xray",
		"/opt/bin/xray",
		"/usr/sbin/xray",
	}
	// Закваски лежат в каталоге пакета; путь в /opt/etc встречается
	// в старых установках.
	TagsCandidates = []string{
		"/opt/apps/kvas/etc/conf/tags.list",
		"/opt/etc/tags.list",
	}
	XrayInitCandidates = []string{
		"/opt/etc/init.d/S24xray",
		"/opt/apps/kvas/etc/init.d/S97xray",
		"/opt/etc/init.d/S97xray",
	}
)
