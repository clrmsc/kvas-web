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

# Внутри opkg работает tar из busybox: он понимает только старый формат
# ustar. Заголовки pax, которые tar на macOS пишет даже с --format=ustar,
# он принимает за мусор и молча не распаковывает файлы, а метаданные macOS
# попадают в архив отдельными файлами «._имя». Поэтому архивы собираются
# python-ом, где формат задаётся явно.
command -v python3 >/dev/null 2>&1 || { echo "нужен python3 для сборки пакета" >&2; exit 1; }

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
# Упаковка
# ------------------------------------------------------------------

# pack_ipk <рабочий каталог> <корень файлов пакета> <итоговый файл>
pack_ipk() {
	python3 - "$1" "$2" "$3" <<'PYPACK'
import os, sys, tarfile

work, root, out = sys.argv[1], sys.argv[2], sys.argv[3]


def add_tree(tf, base):
    """Кладёт дерево файлов от имени root и в порядке, устойчивом между сборками."""
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames.sort()
        entries = [(dirpath, True)] if dirpath != base else [(base, True)]
        entries += [(os.path.join(dirpath, name), False) for name in sorted(filenames)]
        for path, is_dir in entries:
            rel = os.path.relpath(path, base)
            name = "./" if rel == "." else "./" + rel
            if os.path.basename(path).startswith("._"):
                continue  # метаданные macOS в пакет не нужны
            info = tf.gettarinfo(path, arcname=name)
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            if is_dir:
                info.type = tarfile.DIRTYPE
                tf.addfile(info)
            elif info.isfile():
                with open(path, "rb") as f:
                    tf.addfile(info, f)


def make_archive(path, base):
    # USTAR_FORMAT — тот самый старый формат, который понимает busybox.
    with tarfile.open(path, "w:gz", format=tarfile.USTAR_FORMAT) as tf:
        add_tree(tf, base)


make_archive(os.path.join(work, "control.tar.gz"), os.path.join(work, "control"))
make_archive(os.path.join(work, "data.tar.gz"), root)

with tarfile.open(out, "w:gz", format=tarfile.USTAR_FORMAT) as tf:
    for name in ("debian-binary", "control.tar.gz", "data.tar.gz"):
        path = os.path.join(work, name)
        info = tf.gettarinfo(path, arcname="./" + name)
        info.uid = info.gid = 0
        info.uname = info.gname = "root"
        with open(path, "rb") as f:
            tf.addfile(info, f)
PYPACK
}

# ------------------------------------------------------------------
# Проверка готового пакета
# ------------------------------------------------------------------

# Пакет, который busybox не сможет распаковать, выглядит как успешно
# собранный, поэтому проверяем формат до публикации.
verify_package() {
	command -v python3 >/dev/null 2>&1 || {
		echo "  (python3 не найден, проверка формата пропущена)" >&2
		return 0
	}
	python3 - "$1" <<'PYCHECK'
import gzip, io, sys, tarfile

path = sys.argv[1]
problems = []


def raw_headers(data):
    """Возвращает типы записей архива, читая заголовки напрямую.

    tarfile сам поглощает заголовки pax и наружу их не показывает, поэтому
    единственный способ убедиться, что их нет, — просмотреть сырые блоки.
    """
    types = []
    offset = 0
    while offset + 512 <= len(data):
        block = data[offset:offset + 512]
        if block == b"\0" * 512:
            break
        name = block[:100].rstrip(b"\0").decode("utf-8", "replace")
        typeflag = block[156:157].decode("ascii", "replace")
        try:
            size = int(block[124:136].rstrip(b"\0 ").decode() or "0", 8)
        except ValueError:
            size = 0
        types.append((typeflag, name))
        offset += 512 + (size + 511) // 512 * 512
    return types


def check(data, label):
    for typeflag, name in raw_headers(data):
        if typeflag in ("x", "g"):
            problems.append(f"{label}: заголовок pax ({name}) — busybox его не разберёт")
            break
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:
        names = tf.getnames()
    for name in names:
        if name.startswith("._") or "/._" in name:
            problems.append(f"{label}: метаданные macOS в архиве ({name})")
            break
    return names


with open(path, "rb") as f:
    outer_data = gzip.decompress(f.read())

names = check(outer_data, "пакет")
for required in ("./debian-binary", "./control.tar.gz", "./data.tar.gz"):
    if required not in names:
        problems.append(f"в пакете нет {required}")

with tarfile.open(fileobj=io.BytesIO(outer_data)) as outer:
    for member in ("./control.tar.gz", "./data.tar.gz"):
        if member not in names:
            continue
        inner_data = gzip.decompress(outer.extractfile(member).read())
        inner_names = check(inner_data, member)
        if member == "./data.tar.gz":
            for required in ("./opt/apps/kvas/bin/kvas", "./opt/apps/kvas/bin/kvasweb"):
                if required not in inner_names:
                    problems.append(f"в data.tar.gz нет {required}")

if problems:
    print("\n".join(f"  ОШИБКА: {p}" for p in problems), file=sys.stderr)
    sys.exit(1)
PYCHECK
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

	echo "2.0" > "${work}/debian-binary"

	mkdir -p "${OUT_DIR}"
	package="${OUT_DIR}/kvas_${PKG_VERSION}-${PKG_RELEASE}_${arch}.ipk"
	pack_ipk "${work}" "${root}" "${package}"
	rm -rf "${work}"

	verify_package "${package}" || return 1

	echo "${package} ($(du -h "${package}" | cut -f1))"
}

echo "Версия пакета: ${PKG_VERSION}-${PKG_RELEASE}"
for arch in ${ARCHES}; do
	build_one "${arch}"
done
