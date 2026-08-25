#!/bin/sh
#
# Установка Кваса с веб-интерфейсом на роутер Keenetic с Entware.
#
# Ставит пакет целиком: сам Квас, его зависимости и веб-интерфейс.
# Если Квас уже стоит, пакет обновится, а настройки и списки доменов
# сохранятся — их резервная копия делается перед установкой.
#
#   opkg install curl
#   curl -fsSL https://raw.githubusercontent.com/clrmsc/kvas-web/main/install.sh | sh
#
# Лицензия: Apache License 2.0

set -e

REPO="clrmsc/kvas-web"

BLUE="\033[36m"
RED="\033[31m"
NOCL="\033[m"

say()  { printf "%b\n" "$*"; }
fail() { printf "%b\n" "${RED}Ошибка:${NOCL} $*" >&2; exit 1; }

# ------------------------------------------------------------------
# Проверки окружения
# ------------------------------------------------------------------

command -v opkg >/dev/null 2>&1 || fail "не найден opkg.
Скрипт запускается в Entware на роутере: ssh root@<адрес роутера> -p 222"

command -v curl >/dev/null 2>&1 || fail "не найден curl. Установите: opkg install curl"

[ -d /opt/etc ] || fail "не найден каталог /opt/etc — Entware установлен неполностью"

# ------------------------------------------------------------------
# Архитектура
# ------------------------------------------------------------------

detect_arch() {
	arch_line=$(opkg print-architecture 2>/dev/null | awk '$2 != "all" && $2 != "noarch" {print $2}' | tail -1)

	case "${arch_line}" in
		mipsel*)  echo "mipsle";  return ;;
		mips*)    echo "mips";    return ;;
		aarch64*) echo "aarch64"; return ;;
		armv7*|arm*) echo "armv7"; return ;;
	esac

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

	fail "не удалось определить архитектуру роутера ($(uname -m))"
}

ARCH=$(detect_arch)
say "Архитектура роутера: ${BLUE}${ARCH}${NOCL}"

# ------------------------------------------------------------------
# Резервная копия прежней установки
# ------------------------------------------------------------------

if [ -x /opt/apps/kvas/bin/kvas ]; then
	say "Квас уже установлен — сохраняем настройки..."
	/opt/apps/kvas/bin/kvas backup >/dev/null 2>&1 || say "  (резервную копию сделать не удалось, продолжаем)"
	[ -x /opt/etc/init.d/S99kvasweb ] && /opt/etc/init.d/S99kvasweb stop >/dev/null 2>&1
elif opkg list-installed 2>/dev/null | grep -q "^kvas "; then
	# Пакет числится установленным, а файлов нет: следы неудачной установки.
	# opkg откажется ставить поверх, поэтому сначала убираем запись.
	say "Найдена неполная установка — убираем её..."
	opkg remove kvas --force-depends >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------
# Загрузка и установка
# ------------------------------------------------------------------

say "Обновляем список пакетов Entware..."
opkg update >/dev/null 2>&1 || say "  (opkg update завершился с ошибкой, продолжаем)"

TMP_IPK="/tmp/kvas-${ARCH}.$$.ipk"
URL="https://github.com/${REPO}/releases/latest/download/kvas-${ARCH}.ipk"

say "Скачиваем пакет..."
curl -fsSL --connect-timeout 15 "${URL}" -o "${TMP_IPK}" \
	|| fail "не удалось скачать ${URL}"

SIZE=$(wc -c < "${TMP_IPK}" 2>/dev/null || echo 0)
[ "${SIZE}" -gt 1000000 ] || {
	rm -f "${TMP_IPK}"
	fail "скачался неполный файл (${SIZE} байт). Проверьте, что релиз опубликован."
}

say "Устанавливаем (зависимости подтянутся автоматически)..."
# Ставим именно этот файл, поэтому переустановку и понижение версии
# разрешаем сразу: без этого opkg на совпадающей версии ответил бы
# «up to date» и молча ничего не сделал.
opkg install --force-reinstall --force-downgrade "${TMP_IPK}" \
	|| { rm -f "${TMP_IPK}"; fail "opkg не смог установить пакет"; }
rm -f "${TMP_IPK}"

# ------------------------------------------------------------------
# Проверка установки
#
# opkg сообщает об успехе даже когда распаковка провалилась, поэтому
# смотрим на файлы, а не на код возврата.
# ------------------------------------------------------------------

for required in /opt/apps/kvas/bin/kvas /opt/apps/kvas/bin/kvasweb /opt/etc/init.d/S99kvasweb; do
	[ -f "${required}" ] || fail "установка прошла не полностью: нет ${required}.
Посмотрите сообщения opkg выше и повторите установку."
done

chmod 755 /opt/apps/kvas/bin/kvasweb /opt/etc/init.d/S99kvasweb 2>/dev/null || true

/opt/apps/kvas/bin/kvasweb -version >/dev/null 2>&1 \
	|| fail "веб-интерфейс не запускается на этом роутере — возможно, скачан пакет не той архитектуры"

# Веб-интерфейс поднимается из postinst; если он не стартовал, пробуем ещё раз.
if ! /opt/etc/init.d/S99kvasweb check >/dev/null 2>&1; then
	/opt/etc/init.d/S99kvasweb start >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------
# Итог
# ------------------------------------------------------------------

PORT=$(grep "^WEB_PORT=" /opt/etc/kvas.conf 2>/dev/null | tail -1 | cut -d= -f2)
[ -z "${PORT}" ] && PORT=8085

IP=$(/opt/sbin/ip -o -4 addr show br0 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
[ -z "${IP}" ] && IP=$(hostname 2>/dev/null)
[ -z "${IP}" ] && IP="192.168.1.1"

printf "%78s\n" | tr " " "="
say "Квас с веб-интерфейсом установлен."
say ""
say "  1. Настройте пакет:  ${BLUE}kvas setup${NOCL}"
say "     Мастер спросит про интерфейс VPN и включит нужные службы."
say ""
say "  2. Откройте в браузере:  ${BLUE}http://${IP}:${PORT}/${NOCL}"
say "     При первом входе задайте пароль администратора."
say ""
if /opt/etc/init.d/S99kvasweb check >/dev/null 2>&1; then
	say "  Веб-интерфейс уже работает."
else
	say "  ${RED}Веб-интерфейс не запустился${NOCL} — посмотрите /opt/var/log/kvas-web.log"
fi
say ""
say "  Управление веб-интерфейсом:  ${BLUE}kvas web {status|restart|off}${NOCL}"
say "  Журнал:                      ${BLUE}/opt/var/log/kvas-web.log${NOCL}"
printf "%78s\n" | tr " " "="
