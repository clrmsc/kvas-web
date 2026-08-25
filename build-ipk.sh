#!/bin/sh
#
# Сборка пакета ipk для Entware без buildroot.
#
# Формат ipk у Entware — обычный tar.gz с тремя файлами внутри
# (debian-binary, control.tar.gz, data.tar.gz), поэтому весь пакет
# собирается штатным tar. Тексты postinst/prerm/postrm берутся прямо
# из Makefile, чтобы не расходились с описанием пакета.
#
# Использование:
#   sh build-ipk.sh              # пакеты для всех архитектур
#   sh build-ipk.sh mipsle       # только для одной
#
# Лицензия: Apache License 2.0

set -e

cd "$(dirname "$0")"

OUT_DIR="ipk"
WEB_BUILD="web/build"
ARCHES="${1:-mipsle mips aarch64 armv7}"

PKG_VERSION=$(sed -n 's/^PKG_VERSION:=//p' Makefile | tr -d ' ')
PKG_RELEASE=$(sed -n 's/^PKG_RELEASE:=//p' Makefile | tr -d ' ')
[ -n "${PKG_VERSION}" ] || { echo "не удалось прочитать PKG_VERSION из Makefile" >&2; exit 1; }

# Список зависимостей берём из строки DEPENDS Makefile: там он в формате
# «+пакет +пакет», а control ждёт перечисление через запятую.
DEPENDS=$(grep -m1 '^	DEPENDS:=' Makefile \
	| sed 's/^	DEPENDS:=//' \
	| tr ' ' '\n' | sed 's/^+//' | grep -v '^$' \
	| paste -sd, - | sed 's/,/, /g')

# ------------------------------------------------------------------
# Скрипты сопровождения из Makefile
# ------------------------------------------------------------------

extract_script() {
	# $1 — имя блока (postinst, prerm, postrm)
	awk -v name="$1" '
		$0 ~ "^define Package/kvas/" name "$" { inside = 1; next }
		inside && /^endef$/ { exit }
		inside { print }
	' Makefile \
		| sed "s/\$(PKG_VERSION)/${PKG_VERSION}/g; s/\$(PKG_RELEASE)/${PKG_RELEASE}/g; s/\\\$\\\$/\$/g"
}

# ------------------------------------------------------------------
# Сборка одного пакета
# ------------------------------------------------------------------

build_one() {
	arch="$1"
	binary="${WEB_BUILD}/kvasweb-${arch}"

	[ -f "${binary}" ] || {
		echo "не найден ${binary} — соберите: make -C web all" >&2
		return 1
	}

	work=$(mktemp -d)
	root="${work}/data"

	# Раскладка файлов повторяет Package/kvas/install из Makefile.
	mkdir -p "${root}/opt/apps/kvas" "${root}/opt/etc/init.d" \
		"${root}/opt/etc/ndm/fs.d" "${root}/opt/etc/ndm/netfilter.d"

	cp -R opt/. "${root}/opt/apps/kvas/"
	cp opt/etc/ndm/fs.d/15-kvas-start.sh "${root}/opt/etc/ndm/fs.d/"
	cp opt/etc/ndm/netfilter.d/100-dns-local "${root}/opt/etc/ndm/netfilter.d/"
	cp opt/etc/init.d/S96kvas opt/etc/init.d/S99kvasweb "${root}/opt/etc/init.d/"
	[ -f install_hysteria.sh ] && cp install_hysteria.sh "${root}/opt/apps/kvas/bin/"

	cp "${binary}" "${root}/opt/apps/kvas/bin/kvasweb"
	chmod 755 "${root}/opt/apps/kvas/bin/kvasweb" \
		"${root}/opt/etc/init.d/S96kvas" "${root}/opt/etc/init.d/S99kvasweb"
	find "${root}/opt/apps/kvas/bin" -type f -exec chmod 755 {} +

	# control
	mkdir -p "${work}/control"
	cat > "${work}/control/control" <<CONTROL
Package: kvas
Version: ${PKG_VERSION}-${PKG_RELEASE}
Depends: ${DEPENDS}
Source: https://github.com/clrmsc/kvas-web
SourceName: kvas
Section: utils
Architecture: all
Installed-Size: $(du -sk "${root}" | cut -f1)000
URL: https://github.com/clrmsc/kvas-web
Description:  Селективная маршрутизация для Keenetic с веб-интерфейсом.
 Форк проекта Квас: домены из списка идут через VPN, остальное — напрямую.
 Управление из браузера, суточный автовыбор самого быстрого сервера
 из подписки VLESS. Бинарник веб-интерфейса собран под ${arch}.
CONTROL

	for script in postinst prerm postrm; do
		extract_script "${script}" > "${work}/control/${script}"
		# Пустой блок означает, что скрипт не нужен.
		if [ -s "${work}/control/${script}" ]; then
			chmod 755 "${work}/control/${script}"
			sh -n "${work}/control/${script}" || {
				echo "${script} из Makefile содержит синтаксическую ошибку" >&2
				return 1
			}
		else
			rm -f "${work}/control/${script}"
		fi
	done

	# Упаковка
	( cd "${work}/control" && tar czf ../control.tar.gz --uid 0 --gid 0 ./* )
	( cd "${root}" && tar czf ../data.tar.gz --uid 0 --gid 0 ./* )
	echo "2.0" > "${work}/debian-binary"

	mkdir -p "${OUT_DIR}"
	package="${OUT_DIR}/kvas_${PKG_VERSION}-${PKG_RELEASE}_${arch}.ipk"
	( cd "${work}" && tar czf pkg.tar.gz --uid 0 --gid 0 ./debian-binary ./control.tar.gz ./data.tar.gz )
	mv "${work}/pkg.tar.gz" "${package}"
	rm -rf "${work}"

	echo "${package} ($(du -h "${package}" | cut -f1))"
}

echo "Версия пакета: ${PKG_VERSION}-${PKG_RELEASE}"
for arch in ${ARCHES}; do
	build_one "${arch}"
done
