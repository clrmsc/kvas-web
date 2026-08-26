package kvas

import "strings"

// SetupFinished сообщает, прошёл ли Квас первичную настройку.
// До неё команды CLI уходят в интерактивный мастер, а через веб-интерфейс
// ответить ему нечем: операцию нужно отклонить с понятным объяснением,
// а не подвешивать процесс на роутере.
func SetupFinished(confPath string) bool {
	conf, err := Conf{Path: confPath}.GetAll()
	if err != nil {
		// Файла нет или он не читается — считаем, что настройки не было.
		return false
	}
	switch strings.ToLower(strings.TrimSpace(conf["SETUP_FINISHED"])) {
	case "true", "yes", "1":
		return true
	}
	return false
}
