> ## Это форк
>
> Форк [qzeleza/kvas](https://github.com/qzeleza/kvas) (Apache 2.0), отделившийся
> на версии `1.1.9-beta-10`. Отличие от оригинала — веб-интерфейс: вместо
> HTTP-сервера на socat с обработчиками на shell здесь один статический
> бинарник на Go со вшитым интерфейсом, нормальной аутентификацией и защитой
> от инъекций. Плюс подписка на серверы VLESS с суточным выбором самого
> быстрого. Подробности — в [web/README.md](web/README.md).
>
> ### Установка
>
> По SSH в Entware (`ssh root@<адрес роутера> -p 222`).
>
> **Если Кваса на роутере ещё нет** — ставится всё сразу: сам Квас,
> зависимости и веб-интерфейс.
>
> ```sh
> opkg install curl
> curl -fsSL https://raw.githubusercontent.com/clrmsc/kvas-web/main/install.sh | sh
> ```
>
> Дальше — `kvas setup` для первичной настройки и браузер на `http://<адрес роутера>:8085/`.
>
> **Если Квас уже стоит** (в том числе оригинальный) и нужен только
> веб-интерфейс — он добавится, не трогая сам пакет:
>
> ```sh
> curl -fsSL https://raw.githubusercontent.com/clrmsc/kvas-web/main/install-web.sh | sh
> ```
>
> При первом входе браузер попросит задать пароль администратора.
> Управление: `kvas web {status|restart|off}` или
> `/opt/etc/init.d/S99kvasweb`, журнал — `/opt/var/log/kvas-web.log`.
>
> ### Обновления
>
> Веб-интерфейс сам проверяет, не вышла ли новая сборка: **Настройки →
> Обновление Кваса**. Установка занимает около минуты, страница
> перезагрузится сама; настройки, списки и подписка сохраняются. Из консоли
> то же самое делает `kvasweb -update`.
>
> ### Версия xray имеет значение
>
> Провайдеры обновляют серверную часть Reality, и старый клиент перестаёт
> договариваться с частью серверов: в журнале это выглядит как
> `REALITY: received real certificate`, а в списке — «туннель не работает».
> В Entware `xray` нередко отстаёт на несколько версий. Текущая версия
> показана на странице состояния; сверьте её с
> [последним релизом Xray-core](https://github.com/XTLS/Xray-core/releases)
> и при заметном отставании замените бинарник вручную:
>
> ```sh
> cp /opt/sbin/xray /opt/sbin/xray.bak   # на случай отката
> # скачать Xray-linux-<арх>.zip с github.com/XTLS/Xray-core/releases,
> # распаковать и положить xray в /opt/sbin/, затем:
> chmod 755 /opt/sbin/xray && /opt/etc/init.d/S24xray restart
> ```
>
> Учтите: `opkg upgrade` вернёт версию из репозитория Entware.
>
> ### Сборка и выкладка
>
> Пакет собирается без Entware buildroot: `make -C web all` собирает
> бинарники веб-интерфейса, `sh build-ipk.sh` — сами ipk под каждую
> архитектуру. Публикация — `sh release.sh`: соберёт, выложит в релиз и
> проверит, что в нём оказались все файлы (при ручной выкладке
> `gh release upload --clobber` может удалить старые пакеты и прерваться,
> оставив релиз без них).
>
> Не забывайте поднимать `PKG_RELEASE` в Makefile: по нему роутеры видят
> обновление, а opkg отказывается ставить пакет с той же версией.

![GitHub Repo stars](https://img.shields.io/github/stars/qzeleza/kvas?color=orange) ![GitHub closed issues](https://img.shields.io/github/issues-closed/qzeleza/kvas?color=success) ![GitHub last commit](https://img.shields.io/github/last-commit/qzeleza/kvas) ![GitHub commit activity](https://img.shields.io/github/commit-activity/y/qzeleza/kvas) ![GitHub top language](https://img.shields.io/github/languages/top/qzeleza/kvas) ![GitHub code size in bytes](https://img.shields.io/github/languages/code-size/qzeleza/kvas) 
# [КВАС](https://forum.keenetic.com/topic/14415-пробуем-квас-shadowsocks-и-другие-vpn-клиенты) - защита ваших подключений #

---

#### Внимание! 
Открыта [группа в Телеграм](https://t.me/kvas_pro) с целью оперативного обмена информацией по проекту. 

---


### VPN и SHADOWSOCKS клиент для [роутеров Keenetic](https://keenetic.ru/ru/)

#### Пакет представляет собой обвязку или интерфейс командной строки для защиты Вашего соединения при обращении к определенным доменам.

#### В пакете реализуется связка: **ipset** + один из вариантов связки DNS сервера:
- **dnsmasq (с поддержкой wildcard)** + **dnscrypt-proxy2** + блокировщик рекламы **adblock** или
- **AdGuardHome** (уже включает в себя и шифрование **DNS** трафика и блокировщик рекламы).

> В связи с использованием в пакете утилиты dnsmasq с **wildcard**, можно работать с любыми доменными именами третьего и выше уровней. 
> Т.е. в белый список достаточно добавить **domen.com** и маршрутизация трафика 
> будет идти как к **sub1.domen.com**, так и к любому другому поддоменному имени типа **subN.domen.com**.


## Возможности
1. **Квас** работает на всех платформах произведенных **Keenetic** устройств, ввиду легковесности задействованных пакетов: **mips, mipsel, aarch64**.
2. **Квас** использует **dnsmasq**, **с поддержкой регулярных выражений**, а это в свою очередь дает одно, но большое преимущество: можно работать с соцсетями и прочими высоко-нагруженными сайтами, добавив лишь корневые домены по этим сайтам.
3. **Квас** позволяет **отображать статус/отключать/включать** блокировку рекламы (модуль **adblock** + **dnsmasq**);
4. **Квас** позволяет **отображать статус/отключать/включать** шифрование **DNS** (пакет **dnscrypt-proxy2**);
5. **Квас** позволяет тестировать и выводить отладочную информацию по всем элементам связки **ipset + (dnsmasq + dnscrypt-proxy2) | AdGuardHome**
6. **Квас** позволяет подключить **AdGuardHome** в качестве **DNS** сервера, вместо связки **dnsmasq + dnscrypt-proxy2 + adblock**.
7. **Квас** позволяет оперировать со списком исключений при блокировке рекламы, добавляет и удаляет домены в этом списке.

## Установка пакета 
1. Зайдите в **entware** своего роутера и введите команду `opkg install curl && curl -sOfL http://kvas.zeleza.ru/install && sh install`. 
2. Далее, следуйте инструкциям на экране.
3. Подробности читайте [здесь](https://github.com/qzeleza/kvas/wiki/Установка-пакета)

## Используемые в проекте продукты
- Для проведения тестов, в проекте используется пакет [BATS](https://github.com/bats-core/bats-core/blob/master/LICENSE.md) от нескольких [АВТОРОВ](https://github.com/bats-core/bats-core/blob/master/AUTHORS).

## Помощь проекту
Помочь можно переводом средств на [этот кошелек ЮМани](https://yoomoney.ru/to/4100117756734493).

## Документация по проекту
- [Перейти по ссылке](https://github.com/qzeleza/kvas/wiki).

## Каталог всех версий проекта
- [Перейти по ссылке](https://github.com/qzeleza/kvas/tree/main/ipk)

## История "Звезд"

[![Star History Chart](https://api.star-history.com/svg?repos=qzeleza/kvas&type=Timeline)](https://star-history.com/#qzeleza/kvas&Timeline)

--- 

