# Сборка веб-интерфейса Кваса под архитектуры роутеров.
#
# Результат — один статический бинарник без внешних зависимостей:
# интерфейс вшит внутрь, libc не нужна.

BINARY  := kvasweb
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
OUT     := build

# CGO выключен — иначе бинарник потянет за собой libc роутера.
GOFLAGS := CGO_ENABLED=0
LDFLAGS := -s -w -X main.version=$(VERSION)

# Целевые платформы роутеров с Entware:
#   mipsle softfloat — mt7621 (KN-2710 Peak и большинство моделей на MIPS)
#   mips   softfloat — модели с big-endian MIPS
#   aarch64          — Keenetic на ARM64
#   armv7            — старые ARM-модели

# Цели бинарников объявлены .PHONY намеренно: у них нет списка зависимостей,
# и без этого make считал бы уже существующий файл актуальным и не пересобирал
# его после правок в исходниках. Go кеширует сборку сам, так что это дёшево.
.PHONY: all test lint clean host \
	$(OUT)/$(BINARY)-mipsle $(OUT)/$(BINARY)-mips \
	$(OUT)/$(BINARY)-aarch64 $(OUT)/$(BINARY)-armv7

all: test $(OUT)/$(BINARY)-mipsle $(OUT)/$(BINARY)-mips $(OUT)/$(BINARY)-aarch64 $(OUT)/$(BINARY)-armv7

test:
	go test ./...

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

# Бинарник для машины разработчика — для локального стенда.
host:
	$(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $(OUT)/$(BINARY) ./cmd/kvasweb

$(OUT)/$(BINARY)-mipsle:
	$(GOFLAGS) GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kvasweb

$(OUT)/$(BINARY)-mips:
	$(GOFLAGS) GOOS=linux GOARCH=mips GOMIPS=softfloat \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kvasweb

$(OUT)/$(BINARY)-aarch64:
	$(GOFLAGS) GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kvasweb

$(OUT)/$(BINARY)-armv7:
	$(GOFLAGS) GOOS=linux GOARCH=arm GOARM=7 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kvasweb

clean:
	rm -rf $(OUT)
