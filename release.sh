#!/bin/sh
#
# Публикация сборки: собирает пакеты, выкладывает их в релиз и проверяет,
# что в нём оказалось ровно то, что нужно.
#
# Отдельный скрипт появился не от любви к автоматизации: при ручной выкладке
# `gh release upload --clobber` сначала удаляет файлы с такими же именами, и
# если загрузка после этого прервётся, релиз останется без пакетов. Здесь
# состав проверяется после каждой выкладки.
#
# Использование: sh release.sh [тег]
#
# Лицензия: Apache License 2.0

set -e

cd "$(dirname "$0")"

REPO="clrmsc/kvas-web"
TAG="${1:-v1.1.9-web1}"
ARCHES="mipsle mips aarch64 armv7"

command -v gh >/dev/null 2>&1 || { echo "нужен gh" >&2; exit 1; }

PKG_VERSION=$(sed -n 's/^PKG_VERSION:=//p' Makefile | tr -d ' ')
PKG_RELEASE=$(sed -n 's/^PKG_RELEASE:=//p' Makefile | tr -d ' ')
VERSION="${PKG_VERSION}-${PKG_RELEASE}"

echo "Версия: ${VERSION}, тег: ${TAG}"

# ------------------------------------------------------------------
# Сборка
# ------------------------------------------------------------------

echo "Собираем веб-интерфейс..."
make -C web all >/dev/null

echo "Собираем пакеты..."
rm -f ipk/kvas_"${PKG_VERSION}"-*.ipk ipk/version-*
sh build-ipk.sh >/dev/null

# ------------------------------------------------------------------
# Подготовка файлов релиза
# ------------------------------------------------------------------

STAGE=$(mktemp -d)
trap 'rm -rf "${STAGE}"' EXIT

for arch in ${ARCHES}; do
	src="ipk/kvas_${VERSION}_${arch}.ipk"
	[ -f "${src}" ] || { echo "не собран ${src}" >&2; exit 1; }
	cp "${src}" "${STAGE}/kvas-${arch}.ipk"
	cp "web/build/kvasweb-${arch}" "${STAGE}/"
done
cp ipk/version-"${VERSION}" "${STAGE}/"

# ------------------------------------------------------------------
# Выкладка
# ------------------------------------------------------------------

echo "Выкладываем..."
# Прежние метки версий убираем: иначе в релизе накопятся несколько,
# и сервис не поймёт, какая настоящая.
gh api "repos/${REPO}/releases/tags/${TAG}" --jq '.assets[].name' 2>/dev/null \
	| grep '^version-' | grep -v "^version-${VERSION}$" \
	| while read -r old; do
		echo "  убираем прежнюю метку ${old}"
		gh release delete-asset "${TAG}" "${old}" --repo "${REPO}" --yes >/dev/null 2>&1 || true
	done

gh release upload "${TAG}" --repo "${REPO}" --clobber "${STAGE}"/* >/dev/null

# ------------------------------------------------------------------
# Проверка
# ------------------------------------------------------------------

echo "Проверяем состав релиза..."
published=$(gh api "repos/${REPO}/releases/tags/${TAG}" --jq '.assets[].name')

missing=""
for arch in ${ARCHES}; do
	echo "${published}" | grep -qx "kvas-${arch}.ipk"  || missing="${missing} kvas-${arch}.ipk"
	echo "${published}" | grep -qx "kvasweb-${arch}"   || missing="${missing} kvasweb-${arch}"
done
echo "${published}" | grep -qx "version-${VERSION}" || missing="${missing} version-${VERSION}"

if [ -n "${missing}" ]; then
	echo "В релизе не хватает:${missing}" >&2
	exit 1
fi

# Пакет должен скачиваться: битая ссылка равносильна отсутствию релиза.
size=$(gh api "repos/${REPO}/releases/tags/${TAG}" \
	--jq '.assets[] | select(.name=="kvas-aarch64.ipk") | .size')
[ "${size}" -gt 1000000 ] 2>/dev/null || { echo "kvas-aarch64.ipk подозрительно мал: ${size}" >&2; exit 1; }

echo "Готово: в релизе ${TAG} версия ${VERSION}"
