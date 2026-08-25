// Package ui содержит собранный интерфейс, вшитый в исполняемый файл:
// на роутере не нужно ни отдельного каталога с html, ни веб-сервера.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var files embed.FS

// FS возвращает файловую систему с корнем в каталоге dist.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err) // каталог вшит на этапе сборки, ошибка невозможна
	}
	return sub
}
