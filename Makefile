include $(TOPDIR)/rules.mk

PKG_NAME:=kvas
PKG_VERSION:=1.1.9_beta-10
PKG_RELEASE:= 49
PKG_BUILD_DIR:=$(BUILD_DIR)/$(PKG_NAME)-$(PKG_VERSION)-$(PKG_RELEASE)

# Веб-интерфейс — отдельный статический бинарник, который собирается заранее
# командой `make -C web all` (см. web/README.md). Здесь только выбирается
# файл под архитектуру целевого роутера.
KVASWEB_SUFFIX:=$(strip \
	$(if $(findstring mipsel,$(ARCH)),mipsle, \
	$(if $(findstring mips,$(ARCH)),mips, \
	$(if $(findstring aarch64,$(ARCH)),aarch64, \
	$(if $(findstring arm,$(ARCH)),armv7,)))))
KVASWEB_BIN:=web/build/kvasweb-$(KVASWEB_SUFFIX)
MOLOT_UNINSTALL:=kvas uninstall full

include $(INCLUDE_DIR)/package.mk

define Package/kvas
	SECTION:=utils
	CATEGORY:=Keendev
	# DEPENDS:=+jq +curl +knot-dig +libpcre +nano-full +cron +bind-dig +dnsmasq-full +ipset +dnscrypt-proxy2 +iptables +libopenssl +shadowsocks-rust   
	DEPENDS:=+libpcre +jq +curl +knot-dig +nano-full +cron +bind-dig +dnsmasq-full +ipset +dnscrypt-proxy2 +iptables +shadowsocks-libev-ss-redir +shadowsocks-libev-config +libmbedtls
	URL:=no
	TITLE:=VPN клиент для обработки запросов по внесению хостов в белый список.
	PKGARCH:=all
endef
# +libstdcpp 
define Package/kvas/description
	Данный пакет позволяет осуществлять контроль и поддерживать в актуальном состоянии
	защищенный список хостов или "Белый список". При обращении к любому хосту из
	этого списка, весь трафик будет идти через любое VPN или через Shadowsocks соединение,
	заранее настроенное на роутере.
endef

define Build/Prepare
endef
define Build/Configure
endef
define Build/Compile
endef

# Во время инсталляции задаем папку в которую будем
# копировать наш скрипт и затем копируем его в эту папку
define Package/kvas/install
	$(INSTALL_DIR) $(1)/opt/etc/init.d
	$(INSTALL_DIR) $(1)/opt/etc/ndm/fs.d
	$(INSTALL_DIR) $(1)/opt/etc/ndm/netfilter.d
	$(INSTALL_DIR) $(1)/opt/apps/kvas

	$(INSTALL_BIN) opt/etc/ndm/fs.d/15-kvas-start.sh $(1)/opt/etc/ndm/fs.d
	$(INSTALL_BIN) opt/etc/ndm/netfilter.d/100-dns-local $(1)/opt/etc/ndm/netfilter.d

	$(INSTALL_BIN) opt/etc/init.d/S96kvas $(1)/opt/etc/init.d
	$(CP) ./opt/. $(1)/opt/apps/kvas
	$(INSTALL_BIN) install_hysteria.sh $(1)/opt/apps/kvas/bin

	# Веб-интерфейс: бинарник и автозапуск.
	@test -x $(KVASWEB_BIN) || { \
		echo "Не найден $(KVASWEB_BIN)."; \
		echo "Соберите веб-интерфейс: make -C web all"; \
		exit 1; \
	}
	$(INSTALL_BIN) $(KVASWEB_BIN) $(1)/opt/apps/kvas/bin/kvasweb
	$(INSTALL_BIN) opt/etc/init.d/S99kvasweb $(1)/opt/etc/init.d
endef

#---------------------------------------------------------------------
# Скрипт создаем, который выполняется после инсталляции пакета
# Задаем в кроне время обновления ip адресов хостов
#---------------------------------------------------------------------
define Package/kvas/postinst

#!/bin/sh

BLUE="\033[36m";
NOCL="\033[m";

print_line()(printf "%83s\n" | tr " " "=")

chmod -R +x /opt/apps/kvas/bin/*
# chmod -R +x /opt/apps/kvas/sbin/dnsmasq/*
chmod -R +x /opt/apps/kvas/etc/init.d/*
chmod -R +x /opt/apps/kvas/etc/ndm/*

ln -sf /opt/apps/kvas/bin/kvas /opt/bin/kvas

# Настройки Кваса не перезаписываем: в kvas.conf лежит результат
# kvas setup (интерфейс туннеля, SETUP_FINISHED) и порт веб-интерфейса.
# Копируем шаблон только при первой установке, иначе дописываем ключи,
# которых в существующем файле ещё нет.
if [ -f /opt/etc/kvas.conf ]; then
	while IFS= read -r _line; do
		case "$_line" in
			[A-Za-z]*=*)
				_key="${_line%%=*}"
				grep -q "^${_key}=" /opt/etc/kvas.conf || echo "$_line" >> /opt/etc/kvas.conf
				;;
		esac
	done < /opt/apps/kvas/etc/conf/kvas.conf
else
	cp -f /opt/apps/kvas/etc/conf/kvas.conf /opt/etc/kvas.conf
fi
[ -f /opt/etc/kvas.list ] || cp -f /opt/apps/kvas/etc/conf/kvas.list /opt/etc/kvas.list
mkdir -p /opt/etc/adblock /opt/etc/dnsmasq.d
cp -f /opt/apps/kvas/etc/conf/adblock.sources /opt/etc/adblock/sources.list
cp -f /opt/apps/kvas/etc/ndm/ndm /opt/apps/kvas/bin/libs/ndm

sed -i "s/\(APP_VERSION=\).*/\1$(PKG_VERSION)/; s/^,//; s/\,/ /g;" "/opt/etc/kvas.conf"
sed -i "s/\(APP_RELEASE=\).*/\1$(PKG_RELEASE)/; s/^,//; s/\,/ /g;" "/opt/etc/kvas.conf"

chmod +x /opt/apps/kvas/bin/kvasweb 2>/dev/null
chmod +x /opt/etc/init.d/S99kvasweb 2>/dev/null
mkdir -p /opt/var/log

# Веб-интерфейс поднимаем сразу: пароль администратора задаётся
# при первом входе в браузере.
/opt/etc/init.d/S99kvasweb start >/dev/null 2>&1

WEB_HOST=$$(/opt/sbin/ip -o -4 addr show br0 2>/dev/null | awk '{print $$4}' | cut -d/ -f1 | head -1)
[ -z "$$WEB_HOST" ] && WEB_HOST=$$(hostname 2>/dev/null)
[ -z "$$WEB_HOST" ] && WEB_HOST="192.168.1.1"

print_line
echo -e "Для настройки пакета КВАС наберите \033[36mkvas setup\033[m"
echo -e "Для поддержки Hysteria 2 наберите \033[36mkvas hysteria help\033[m"
echo -e "Веб-интерфейс: \033[36mhttp://$$WEB_HOST:8085/\033[m — при первом входе задайте пароль"
print_line

endef

#---------------------------------------------------------------------
# Создаем скрипт, который выполняется при удалении пакета
# Удаляем из крона запись об обновлении ip адресов
#---------------------------------------------------------------------
define Package/kvas/prerm

#!/bin/sh

# Останавливаем веб-интерфейс до удаления файлов пакета.
[ -x /opt/etc/init.d/S99kvasweb ] && /opt/etc/init.d/S99kvasweb stop >/dev/null 2>&1

exit 0

endef

define Package/kvas/postrm

#!/bin/sh

# Состояние веб-интерфейса (пароль, подписка) намеренно остаётся на месте:
# opkg вызывает postrm и при обновлении пакета, а не только при удалении,
# поэтому удаление здесь стирало бы настройки на каждом обновлении.
# Чтобы убрать их полностью: rm -rf /opt/etc/kvas-web

exit 0

endef

$(eval $(call BuildPackage,kvas))
