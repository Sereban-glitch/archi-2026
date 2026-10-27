# Archi 2026 — гибридный DevOps-агент на Go

> Мозг на ноутбуке. Руки дотягиваются до продакшен-сервера. Один бинарник.

Архитектурный апгрейд автономного агента «Archi»: миграция рантайма с
задыхающейся GCP VM (1 vCPU / 1 ГБ RAM) на локальный ноутбук (i5-6200U,
12 ГБ RAM, Linux Mint 22.3) с сохранением полного контроля над удалённой
машиной **agrobot-prod** через отказоустойчивый SSH-мост.

## Возможности

- **ReAct-ядро** (Think → Action → Observation → Response) с параллельным
  исполнением инструментов, защитой от зацикливания (max steps + детект
  повторных вызовов) и graceful fallback при деградации LLM.
- **Мост `archibridge`**: телеметрия обеих нод (CPU/RAM/диск/батарея/температуры/
  systemd-сервисы), SSH-пул с keep-alive и экспоненциальным backoff, пороговые
  тревоги (RAM > 85%, CPU > 80%, диск > 90%, упавшие сервисы), watchdog
  автоперезапуска сервисов на VM.
- **Инструменты**: `local_bash` / `remote_bash`, `read_file` / `write_file` /
  `list_directory` (с параметром ноды), `systemd_status` / `systemd_restart`,
  `node_status`, `memory_recall` / `memory_save`, `search_knowledge`.
- **Два интерфейса + голос**: Telegram (long polling с защитой от ECONNRESET,
  голосовые сообщения → STT → агент, опциональный голосовой ответ TTS) и
  локальный CLI через UNIX-сокет (`archi ctl "текст"`, `archi voice` —
  запрос с микрофона с распознаванием и озвучкой ответа).
- **Голосовой движок** (2026.11): OpenAI-совместимый STT-протокол — работает
  с Groq `whisper-large-v3-turbo`, OpenAI `whisper-1` и локальным
  whisper.cpp server без изменения кода; микрофон через arecord/ffmpeg/parecord;
  ноль CGO — статическая сборка сохранена.
- **Долговременная память**: JSONL + keyword-recall + компактизация истории
  (LLM-сводка при переполнении контекста).
- **Zero-dependency**: один статический бинарник ~8 МБ, только stdlib +
  `golang.org/x/crypto` (SSH) + `gopkg.in/yaml.v3`.

## Архитектура

```
┌──────────────────────── НОУТБУК (HP ProBook, Mint 22.3, 12 ГБ) ─────────────────────────┐
│                                                                                          │
│  ┌───────────────┐    ┌────────────────────────── archi (Go-бинарник) ──────────────┐   │
│  │ Telegram API  │◄───┤ internal/telegram: polling (anti-ECONNRESET, backoff+jitter) │   │
│  └───────────────┘   │  Runner: /status /reset, тревоги→админы, сводки 09:00/21:00  │   │
│                      │                                  ▲                            │   │
│  ┌───────────────┐   │  ┌───────────────────────────┐   │                            │   │
│  │ archi ctl CLI │◄──┼──┤ internal/ipc: UNIX-сокет   ├───┤                            │   │
│  │ (терминал)    │   │  │ line-based JSON            │   │                            │   │
│  └───────────────┘   │  └───────────────────────────┘   │                            │   │
│                      │                                  ▼                            │   │
│                      │  ┌──────────────── internal/agent: ReAct-ядро ───────────────┐  │   │
│                      │  │  сессии ▸ промпт с живой телеметрией ▸ LLM-клиент        │  │   │
│                      │  │  (Anthropic Messages API, ретраи, компактизация)         │  │   │
│                      │  │  память (JSONL + skills/) ▸ repeatGuard ▸ maxSteps       │  │   │
│                      │  └──────┬──────────────────────────────────▲───────────────┘  │   │
│                      │         │ tool_use                     tool_result           │   │
│                      │         ▼                                 │                  │   │
│                      │  ┌──────────────── internal/tools: реестр ─┴───────────────┐  │   │
│                      │  │ local_bash │ remote_bash │ read/write_file │ systemd_*  │  │   │
│                      │  │ node_status │ memory_* │ search_knowledge               │  │   │
│                      │  └──────┬──────────────────────────────────▲───────────────┘  │   │
│                      │         │                              │                      │   │
│                      │  ┌──────▼──── internal/bridge: мост ─────┴───────────────┐   │   │
│                      │  │ LocalCollector (/proc,/sys)   Alerter (пороги+cooldown)│   │   │
│                      │  │ RemoteCollector (SSH-скрипт)   Watchdog (авто-рестарт) │   │   │
│                      │  │ SSHPool: keepalive, backoff, reconnection               │   │   │
│                      │  └──────┬─────────────────────────────────────────────────┘   │   │
│                      └─────────┼───────────────────────────────────────────────────┘   │
│                                │                                                        │
│              SSH (ключ, ~/.ssh/config: agrobot-prod)      LLM-туннель (localhost:18080) │
└────────────┬───────────────────────────────────────┬────────────────────────────────────┘
             ▼                                       ▼
┌────────── GCP VM agrobot-prod (Debian, 1 ГБ) ──────────────┐  ┌──────────────────────┐
│  sshd ◄── SSH-пул (3 соединения, keepalive 15 c)           │  │ antigravity-claude-  │
│  systemd: console-bot, postgresql, agrobot, info-bot       │  │ proxy (:18080)       │
│  ► телеметрия одним sh-скриптом: CPU/RAM/disk/сервисы      │  │ Anthropic Messages   │
│  ► удалённые команды (таймаут, лимит вывода 256 KiB)       │  │ API → Gemini/Claude  │
│  ► watchdog: авто-рестарт console-bot при падении          │  └──────────────────────┘
└─────────────────────────────────────────────────────────────┘          ▲
                                                                         │ через SSH-туннель
                                     http://127.0.0.1:18080/v1/messages ──┘
                                     (archi-tunnel.service)
```

## Сборка

```bash
# требуется Go 1.22+
make build          # → bin/archi (статический, ~8 МБ)
make vet            # go vet
make check          # валидация deploy/config.yaml
```

Зависимости: `golang.org/x/crypto` (SSH), `gopkg.in/yaml.v3` (конфиг). Всё остальное — stdlib.

## Развёртывание на ноутбуке

1. **SSH-доступ** — убедись, что `ssh agrobot-prod` работает по ключу
   (Host уже описан в `~/.ssh/config`).
2. **LLM-туннель** (прокси живёт на VM):
   ```bash
   sudo cp deploy/archi-tunnel.service /etc/systemd/system/
   sudo systemctl daemon-reload && sudo systemctl enable --now archi-tunnel
   curl -s http://127.0.0.1:18080/health   # проверить
   ```
3. **Бинарник и конфиг**:
   ```bash
   sudo make deploy        # install + systemd-юниты
   ```
4. **Токен бота и user ID** — в `/etc/archi/archi.env`:
   ```
   TELEGRAM_BOT_TOKEN=123456:ABC...
   ```
   и в `/etc/archi/config.yaml` → `telegram.allowed: [твой-telegram-id]`.
5. **Запуск**: `systemctl status archi`, логи: `journalctl -u archi -f`.

## Использование

```bash
# локально, из терминала (без Telegram):
archi ctl --status
archi ctl "проверь логи console-bot на сервере за последний час"
archi ctl --reset           # сброс контекста IPC-сессии
archi voice                 # голосом с микрофона: запись → STT → агент → ответ
archi voice --speak         # то же + озвучка ответа (нужен voice.tts.enabled)

# в Telegram:
/status   — состояние узлов
/reset    — сброс контекста
<текст>   — всё остальное: «перезапусти info-bot», «сколько RAM свободно на VM?»
```

## Отказоустойчивость (что заложено)

| Сбой                          | Реакция системы                                                        |
|-------------------------------|------------------------------------------------------------------------|
| ECONNRESET / обрыв polling    | backoff 1→60 c с джиттером, автопродолжение, offset не теряется        |
| LLM-прокси недоступен         | 5 ретраев + экспон. backoff → graceful fallback-ответ пользователю     |
| SSH-обрыв / VM легла          | node_down-алерт, пул пересобирается, backoff до 30 c                   |
| Зацикливание ReAct            | max_steps=24 + детект идентичных tool_use → «измени параметры»        |
| Паника в инструменте/обработчике | recover + лог + ответ «восстановлено»                              |
| Падение демона                | systemd Restart=always (5 c)                                           |
| Разрастание контекста         | компактизация: LLM-сводка истории → handoff в долговременную память   |
| Команда-пожиратель вывода     | limitedBuf 256 KiB, обрезка + флаг truncated                            |
| rm -rf / и прочая дичь        | bash_deny_patterns — санитайзер отклоняет до исполнения                |
| Утечка секретов               | forbidden_read_patterns: .ssh, id_rsa, .pem, credentials.json, shadow |

## Структура проекта

```
archi/
├── cmd/archi/main.go           # вход: serve | ctl | check-config | version
├── internal/
│   ├── config/config.go        # YAML + env-подстановка + дефолты + валидация
│   ├── bridge/                 # МОДУЛЬ 1: archibridge
│   │   ├── types.go            #   NodeMetrics, Event, ExecResult, Runner
│   │   ├── local.go            #   LocalCollector (/proc, /sys: CPU, RAM, батарея, терм.)
│   │   ├── remote.go           #   RemoteCollector (один SSH-скрипт → key-value)
│   │   ├── ssh.go              #   SSHPool: keepalive, backoff+jitter, ExitError/EOF
│   │   ├── executor.go         #   Executor: local (pgid+kill группы) / remote
│   │   ├── alerts.go           #   Alerter: пороги + cooldown-дедупликация
│   │   └── bridge.go           #   оркестратор: цикл, снапшот, watchdog, подписчики
│   ├── agent/                  # МОДУЛЬ 2: archicore
│   │   ├── llm.go              #   клиент /v1/messages (Anthropic Tools API)
│   │   ├── agent.go            #   ReAct-цикл, параллельные инструменты, compact()
│   │   ├── session.go          #   сессии с LRU-вытеснением
│   │   ├── memory.go           #   JSONL-память + recall + skills-каталог
│   │   └── prompt.go           #   системный промпт с живой телеметрией
│   ├── tools/                  # реестр инструментов
│   │   ├── registry.go         #   JSON Schema, таймауты, паник-гард, лимит вывода
│   │   ├── bash.go              #   local_bash / remote_bash + санитайзер
│   │   ├── fs.go               #   read/write/list с нодой, base64-передача на VM
│   │   ├── systemd.go           #   systemd_status / systemd_restart / node_status
│   │   └── knowledge.go         #   memory_* / search_knowledge
│   ├── telegram/               # МОДУЛЬ 3a: Telegram-раннер
│   │   ├── bot.go              #   собственный polling-клиент (anti-ECONNRESET)
│   │   └── runner.go            #   команды, тревоги, сводки 09:00/21:00
│   └── ipc/                    # МОДУЛЬ 3b: локальный интерфейс
│       ├── server.go           #   UNIX-сокет, line-JSON, мульти-запросы
│       └── cli.go              #   клиент `archi ctl`
├── deploy/
│   ├── config.yaml             # эталонный конфиг
│   ├── archi.service           # systemd-юнит демона
│   └── archi-tunnel.service    # systemd-юнит SSH-туннеля к LLM-прокси
├── Makefile
└── README.md
```

## Ресурсы

- RAM в простое: ~30–60 МБ (против 240 МБ MemoryMax у старого Node-бота).
- Цикл телеметрии: раз в 30 с (настраивается), SSH-сессия ~1/цикл из пула.
- Бинарник: ~8 МБ статик (CGO_ENABLED=0), старт < 100 мс.
- Локальный whisper-фолбэк (если поднят archi-whisper.service): ~600 МБ в работе,
  0 в простое при выключенном юните — фолбэк просто не сработает и вернётся ошибка основного провайдера.

## Возможности 2026.12 «Автономность»

- **Cron-задания** (`internal/cron`): агент сам просыпается по расписанию
  (`every: 6h` или `times: ["09:05"]`), проходит полный ReAct-цикл в сессии
  `cron:<name>`, отчитывается админам и пишет episode в память. Failure-nudge
  ×3 с backoff 5m (паттерн Hermes Agent), `run_on_start` для пропущенных слотов.
  Валидатор конфига ловит name/prompt/every+times/HH:MM.
- **Approval-клавиатуры**: опасные команды (systemctl restart, rm -r, pkill,
  chmod /etc, iptables, drop table, git push — список в `tools.approval_patterns`)
  исполняются только после «✅ Разрешить» в Telegram. Нет ответа за
  `approval_timeout` / Telegram выключен → отказ (fail-closed). Deny-набор
  усилен (`--no-preserve-root`, `find / -delete`, `curl|sh`, `wget|bash`).
- **Голосовой фолбэк STT**: Groq `whisper-large-v3-turbo` первичный →
  локальный whisper.cpp (`archi-whisper.service`, модель small-q5_0)
  автоматически при недоступности облака. Работает оффлайн.
- **Usage-метрики**: `/status` и `archi ctl --status` показывают запросы и
  токены in/out с старта демона.
- **Аудит-фиксы 2026-10**: digest-слоты, ретраи 429/5xx у LLM, data race
  SSH-пула, `history_limit`, сериализация ReAct-циклов сессии (TOCTOU
  компактизации), UTF-8-безопасные усечения, компакция memory.jsonl,
  symlink-защита writeAllowed, IPC-сессия `cli`.

Подробности — HANDOFF.md.

## Что дальше (roadmap)

- [x] ~~Проактивность: cron-задания~~ (2026.12)
- [x] ~~Approval перед side-effects~~ (2026.12)
- [x] ~~Usage-счётчики токенов~~ (2026.12)
- [ ] `send_file` / `generate_photo` (Gemini) — перенос из старого бота.
- [ ] Векторный recall вместо keyword-поиска (sqlite-vec или чистый Go).
- [ ] Hands-free «Hey Archi»: Silero VAD + openWakeWord + ring-buffer
      (archi-ear.service; CPU-бюджет ~5% ядра).
- [ ] Локальный TTS Piper ru_RU для озвучки по умолчанию.
- [ ] Webhook-режим Telegram как альтернатива polling.
- [ ] Метрики в Prometheus text format на localhost:9101.
