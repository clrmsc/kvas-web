#!/bin/sh
#
# Установка веб-интерфейса Кваса на роутер.
#
# Ставится поверх уже установленного пакета kvas: докачивает бинарник под
# архитектуру роутера, регистрирует автозапуск и поднимает интерфейс.
# Сам Квас при этом не трогается.
#
#   curl -fsSL https://raw.githubusercontent.com/clrmsc/kvas-web/main/install-web.sh | sh
#
# Лицензия: Apache License 2.0

set -e

REPO="clrmsc/kvas-web"
KVAS_DIR="/opt/apps/kvas"
BIN_PATH="${KVAS_DIR}/bin/kvasweb"
INIT_PATH="/opt/etc/init.d/S99kvasweb"
KVAS_CONF="/opt/etc/kvas.conf"
DEFAULT_PORT=8085

BLUE="\033[36m"
RED="\033[31m"
NOCL="\033[m"

say()  { printf "%b\n" "$*"; }
fail() { printf "%b\n" "${RED}Ошибка:${NOCL} $*" >&2; exit 1; }

# ------------------------------------------------------------------
# Проверки окружения
# ------------------------------------------------------------------

[ -x "${KVAS_DIR}/bin/kvas" ] || fail "не найден Квас в ${KVAS_DIR}.
Сначала установите сам пакет: https://github.com/qzeleza/kvas"

command -v curl >/dev/null 2>&1 || fail "не найден curl. Установите: opkg install curl"

# ------------------------------------------------------------------
# Архитектура
# ------------------------------------------------------------------

detect_arch() {
	# Архитектура пакетов Entware — самый надёжный источник: она же
	# различает mips и mipsel, чего uname -m не делает.
	arch_line=$(opkg print-architecture 2>/dev/null | awk '$2 != "all" && $2 != "noarch" {print $2}' | tail -1)

	case "${arch_line}" in
		mipsel*) echo "mipsle"; return ;;
		mips*)   echo "mips";   return ;;
		aarch64*) echo "aarch64"; return ;;
		armv7*|arm*) echo "armv7"; return ;;
	esac

	# Запасной путь: определяем порядок байтов вручную.
	case "$(uname -m)" in
		aarch64) echo "aarch64"; return ;;
		armv7l|armv7|arm*) echo "armv7"; return ;;
		mips*)
			if [ "$(printf 'I' | od -An -tx2 | tr -d ' ')" = "0049" ]; then
				echo "mips"
			else
				echo "mipsle"
			fi
			return
			;;
	esac

	fail "не удалось определить архитектуру роутера ($(uname -m)).
Соберите бинарник вручную: make -C web all"
}

ARCH=$(detect_arch)
say "Архитектура роутера: ${BLUE}${ARCH}${NOCL}"

# ------------------------------------------------------------------
# Загрузка
# ------------------------------------------------------------------

TMP_BIN="/tmp/kvasweb-${ARCH}.$$"
URL="https://github.com/${REPO}/releases/latest/download/kvasweb-${ARCH}"

say "Скачиваем веб-интерфейс..."
curl -fsSL --connect-timeout 15 "${URL}" -o "${TMP_BIN}" \
	|| fail "не удалось скачать ${URL}"

# Бинарник статический и заметно больше мегабайта: так отсеиваем
# страницу с ошибкой, отданную вместо файла.
SIZE=$(wc -c < "${TMP_BIN}" 2>/dev/null || echo 0)
[ "${SIZE}" -gt 1000000 ] || {
	rm -f "${TMP_BIN}"
	fail "скачался неполный файл (${SIZE} байт). Проверьте, что релиз опубликован."
}

chmod 755 "${TMP_BIN}"
"${TMP_BIN}" -version >/dev/null 2>&1 \
	|| { rm -f "${TMP_BIN}"; fail "скачанный бинарник не запускается на этом роутере — возможно, не та архитектура"; }

VERSION=$("${TMP_BIN}" -version 2>/dev/null)

# ------------------------------------------------------------------
# Установка
# ------------------------------------------------------------------

[ -x "${INIT_PATH}" ] && "${INIT_PATH}" stop >/dev/null 2>&1

mkdir -p "${KVAS_DIR}/bin" /opt/var/log /opt/etc/kvas-web
chmod 700 /opt/etc/kvas-web
mv -f "${TMP_BIN}" "${BIN_PATH}"
chmod 755 "${BIN_PATH}"

say "Устанавливаем автозапуск..."
curl -fsSL --connect-timeout 15 \
	"https://raw.githubusercontent.com/${REPO}/main/opt/etc/init.d/S99kvasweb" \
	-o "${INIT_PATH}" || fail "не удалось скачать init-скрипт"
chmod 755 "${INIT_PATH}"

# Настройки веб-интерфейса в общем конфиге Кваса.
if ! grep -q "^WEB_PORT=" "${KVAS_CONF}" 2>/dev/null; then
	{
		echo ""
		echo "# Веб-интерфейс: порт и, при желании, собственный сертификат TLS."
		echo "WEB_PORT=${DEFAULT_PORT}"
		echo "WEB_TLS_CERT="
		echo "WEB_TLS_KEY="
	} >> "${KVAS_CONF}"
fi

PORT=$(grep "^WEB_PORT=" "${KVAS_CONF}" 2>/dev/null | tail -1 | cut -d= -f2)
[ -z "${PORT}" ] && PORT=${DEFAULT_PORT}

# Старая веб-морда на socat заняла бы тот же порт.
if command -v pkill >/dev/null 2>&1; then
	pkill -f "socat.*TCP-LISTEN:${PORT}" >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------
# Запуск
# ------------------------------------------------------------------

say "Запускаем..."
"${INIT_PATH}" start >/dev/null 2>&1 || true
sleep 2

if ! "${INIT_PATH}" check >/dev/null 2>&1; then
	say "${RED}Веб-интерфейс не запустился.${NOCL} Журнал:"
	tail -20 /opt/var/log/kvas-web.log 2>/dev/null
	exit 1
fi

IP=$(/opt/sbin/ip -o -4 addr show br0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
[ -z "${IP}" ] && IP=$(hostname 2>/dev/null)
[ -z "${IP}" ] && IP="192.168.1.1"

printf "%78s\n" | tr " " "="
say "Веб-интерфейс Кваса ${VERSION} установлен."
say ""
say "  Откройте: ${BLUE}http://${IP}:${PORT}/${NOCL}"
say "  При первом входе задайте пароль администратора."
say ""
say "  Управление:  ${BLUE}${INIT_PATH} {start|stop|restart|check}${NOCL}"
say "  Журнал:      ${BLUE}/opt/var/log/kvas-web.log${NOCL}"
printf "%78s\n" | tr " " "="
