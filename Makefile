# ============================================================
# Archi 2026 — Makefile
# Цели: build / vet / test / install / deploy
# ============================================================

BINARY   := archi
VERSION  := $(shell date +%Y.%m.%d 2>/dev/null || echo 2026.10.0)
GO       ?= go
PREFIX   ?= /usr/local
ETCDIR   ?= /etc/archi

LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all build vet test clean install uninstall deploy check

all: vet build

## build: собрать статический бинарник (CGO выключен — zero-dependency)
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/archi
	@ls -la bin/$(BINARY)

## vet: статический анализ
vet:
	$(GO) vet ./...

## test: юнит- и интеграционные тесты (voice: мок-STT, ffmpeg-конверсия, Bot API)
test:
	$(GO) test ./... -count=1

## install: поставить бинарник и дефолтный конфиг
install: build
	install -Dm755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@if [ ! -f $(ETCDIR)/config.yaml ]; then \
	        install -Dm640 deploy/config.yaml $(ETCDIR)/config.yaml; \
	        echo ">>> создан $(ETCDIR)/config.yaml — заполни token и user ID!"; \
	else \
	        echo ">>> $(ETCDIR)/config.yaml уже существует, не трогаю"; \
	fi
	install -d -m 755 /var/lib/archi/memory/skills
	install -d -m 755 /run/archi

## deploy: юниты systemd + enable
deploy: install
	install -Dm644 deploy/archi.service /etc/systemd/system/archi.service
	install -Dm644 deploy/archi-tunnel.service /etc/systemd/system/archi-tunnel.service
	systemctl daemon-reload
	@echo ">>> Запуск: systemctl enable --now archi-tunnel archi"

## check: проверка конфига без запуска
check:
	./bin/$(BINARY) check-config --config $(ETCDIR)/config.yaml || \
	./bin/$(BINARY) check-config --config deploy/config.yaml

clean:
	rm -rf bin/
