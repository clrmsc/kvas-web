#!/bin/sh
# Готовит песочницу для локального стенда: конфигурационные файлы Кваса
# и скрипт-заглушку вместо настоящего CLI. Запускается из dev.sh, но может
# вызываться и отдельно — например, чтобы сбросить стенд в исходное состояние.
set -e

SANDBOX="${KVASWEB_SANDBOX:-/tmp/kvasweb-dev}"
PORT="${PORT:-8099}"

mkdir -p "$SANDBOX/state"

# Заглушка CLI: печатает то, что было вызвано, и правит списки как настоящий Квас.
cat > "$SANDBOX/kvas" <<'FAKE'
#!/bin/sh
LIST="${KVASWEB_SANDBOX_LIST:-/tmp/kvasweb-dev/kvas.list}"
echo "[заглушка] kvas $*"
case "$1" in
	add)
		shift
		for d in "$@"; do
			grep -qxF "$d" "$LIST" 2>/dev/null || echo "$d" >> "$LIST"
			echo "Домен $d добавлен в список."
		done
		;;
	del)
		shift
		for d in "$@"; do
			sed -i.bak "/^$(echo "$d" | sed 's/[.[\*^$]/\\&/g')$/d" "$LIST" 2>/dev/null || true
			echo "Домен $d удалён из списка."
		done
		;;
	import)
		while IFS= read -r d; do
			[ -z "$d" ] && continue
			grep -qxF "$d" "$LIST" 2>/dev/null || echo "$d" >> "$LIST"
			echo "Импортирован $d"
			sleep 0.05
		done < "$2"
		;;
	init|update)
		echo "Пересобираем таблицы…"; sleep 0.4
		echo "Перезапускаем dnsmasq…"; sleep 0.4
		echo "Готово."
		;;
	backup) echo "Копия создана: /opt/kvas_backup_dev" ;;
esac
exit 0
FAKE
chmod +x "$SANDBOX/kvas"

[ -f "$SANDBOX/kvas.list" ] || printf 'youtube.com\nchatgpt.com\nrutracker.org\n' > "$SANDBOX/kvas.list"
[ -f "$SANDBOX/tags.list" ] || printf '[Видео]\nyoutube.com\nvimeo.com\nnetflix.com\n\n[Соцсети]\nfacebook.com\ninstagram.com\n' > "$SANDBOX/tags.list"
[ -f "$SANDBOX/block.list" ] || printf 'ads.example.com\ntracker.example.net\n' > "$SANDBOX/block.list"
[ -f "$SANDBOX/dnsmasq.conf" ] || printf 'port=9753\n' > "$SANDBOX/dnsmasq.conf"
[ -f "$SANDBOX/kvas.conf" ] || printf 'APP_VERSION=1.1.9\nINFACE_ENT=Proxy21\nroute_full_ip=192.168.1.50\nroute_excluded_ip=192.168.1.77\n' > "$SANDBOX/kvas.conf"


# Заглушка xray: обычный SOCKS5 без шифрования. Позволяет проверить замер
# скорости и переключение серверов без роутера.
go build -o "$SANDBOX/fakexray" ./cmd/fakexray

# Заглушка init-скрипта xray.
cat > "$SANDBOX/S97xray" <<'XRAYINIT'
#!/bin/sh
SANDBOX=/tmp/kvasweb-dev
PIDFILE="$SANDBOX/fakexray.pid"

start() {
	[ -f "$SANDBOX/xray.json" ] || { echo "нет конфигурации"; exit 1; }
	"$SANDBOX/fakexray" run -c "$SANDBOX/xray.json" >> "$SANDBOX/fakexray.log" 2>&1 &
	echo $! > "$PIDFILE"
}

stop() {
	[ -f "$PIDFILE" ] && kill "$(cat "$PIDFILE")" 2>/dev/null
	rm -f "$PIDFILE"
}

case "$1" in
	start) start ;;
	stop) stop ;;
	restart) stop; sleep 1; start ;;
	check|status) [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null ;;
	*) echo "использование: $0 {start|stop|restart|check}"; exit 1 ;;
esac
XRAYINIT
chmod +x "$SANDBOX/S97xray"

echo "Песочница готова: $SANDBOX"
