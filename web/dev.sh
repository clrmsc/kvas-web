#!/bin/sh
# Локальный стенд: поднимает веб-интерфейс с песочницей вместо роутера.
# Настоящий CLI Кваса заменяется скриптом-заглушкой, поэтому запускать
# можно на любой машине с Go.
set -e

# Скрипт может быть запущен из корня репозитория — переходим к модулю.
cd "$(dirname "$0")"

SANDBOX="${KVASWEB_SANDBOX:-/tmp/kvasweb-dev}"
PORT="${PORT:-8099}"

sh ./dev-sandbox.sh

export KVASWEB_SANDBOX_LIST="$SANDBOX/kvas.list"

exec go run ./cmd/kvasweb \
	-listen "127.0.0.1:$PORT" \
	-kvas-bin "$SANDBOX/kvas" \
	-kvas-conf "$SANDBOX/kvas.conf" \
	-hosts-list "$SANDBOX/kvas.list" \
	-tags-list "$SANDBOX/tags.list" \
	-adblock-list "$SANDBOX/block.list" \
	-dnsmasq-conf "$SANDBOX/dnsmasq.conf" \
	-failover-conf "$SANDBOX/failover.conf" \
	-state-dir "$SANDBOX/state" \
	-log-file "$SANDBOX/web.log" \
	-xray-bin "$SANDBOX/fakexray" \
	-xray-conf "$SANDBOX/xray.json" \
	-xray-init "$SANDBOX/S97xray"
