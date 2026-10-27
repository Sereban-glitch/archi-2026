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
  `node_status`, `memory_recall` / `memory_save`, `search_knowledge`,
  `skill_save` / `skill_read` / `skill_list`, `delegate_task`, `browser`.
- **Два интерфейса + голос**: Telegram (long polling с защитой от ECONNRESET,
  голосовые сообщения → STT → агент, опциональный голосовой ответ TTS) и
  локальный CLI через UNIX-сокет (`archi ctl "текст"`, `archi voice` —
  запрос с микрофона с распознаванием и озвучкой ответа) + hands-free
  слушатель `archi ear` («Арчи, …» → STT → агент → TTS).
- **Голосовой движок** (2026.11–2026.13): OpenAI-совместимый STT-протокол — работает
  с Groq `whisper-large-v3-turbo`, OpenAI `whisper-1` и локальным
  whisper.cpp server без изменения кода; микрофон через arecord/ffmpeg/parecord;
  TTS — облако (OpenAI/Groq) или локальный Piper (офлайн); ноль CGO —
  статическая сборка сохранена.
- **Долговременная память**: JSONL + гибридный recall (keyword + опциональные
  эмбеддинги через OpenAI-совместимый /embeddings) + компактизация истории
  (LLM-сводка при переполнении контекста) + само-пишущиеся навыки.
- **Observability**: Prometheus text-эндпоинт `/metrics` + `/healthz` на
  localhost (127.0.0.1:9109), счётчики запросов/токенов/инструментов.
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
make build          # → bin/archi (статический, ~8.8 МБ)
make vet            # go vet
make test           # 7 пакетов тестов (включая VAD/wake-word/skills/metrics)
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
6. **Опционально — голос**:
   ```bash
   # STT: ключ Groq в /etc/archi/archi.env (GROQ_API_KEY=...) + voice.stt.enabled: true
   # Локальный фолбэк STT (офлайн): deploy/archi-whisper.service — см. шапку юнита

   # Офлайн-озвучка Piper (голоса ru: irina/ruslan/dmitri):
   sudo bash deploy/setup-piper.sh            # поставит /opt/piper + голос
   # → в конфиге: voice.tts.provider: piper

   # Hands-free «Арчи, …» (требует stt + tts):
   # в конфиге ear.enabled: true, затем:
   sudo cp deploy/archi-ear.service /etc/systemd/system/
   sudo systemctl daemon-reload && sudo systemctl enable --now archi-ear
   journalctl -u archi-ear -f
   ```

## Использование

```bash
# локально, из терминала (без Telegram):
archi ctl --status
archi ctl "проверь логи console-bot на сервере за последний час"
archi ctl --reset           # сброс контекста IPC-сессии
archi voice                 # голосом с микрофона: запись → STT → агент → ответ
archi voice --speak         # то же + озвучка ответа (нужен voice.tts.enabled)
archi ear                   # hands-free демон «Арчи, …» (скрыт от терминала: systemd)

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
│   │   ├── llm.go              #   клиент /v1/messages (Anthropic Tools API) + failover
│   │   ├── agent.go            #   ReAct-цикл, параллельные инструменты, compact()
│   │   ├── session.go           #   сессии с LRU-вытеснением
│   │   ├── memory.go            #   JSONL-память: гибридный recall + компакция
│   │   ├── embed.go             #   /embeddings-векторизатор + косинус
│   │   └── prompt.go            #   системный промпт с живой телеметрией
│   ├── tools/                  # реестр инструментов
│   │   ├── registry.go          #   JSON Schema, таймауты, паник-гард, лимит вывода
│   │   ├── bash.go             #   local_bash / remote_bash + санитайзер
│   │   ├── fs.go                #   read/write/list с нодой, base64-передача на VM
│   │   ├── systemd.go          #   systemd_status / systemd_restart / node_status
│   │   ├── knowledge.go         #   memory_* / search_knowledge
│   │   ├── approval.go         #   политика: deny → approval → исполнение
│   │   ├── skills.go            #   skill_save / skill_read / skill_list
│   │   ├── subagent.go         #   delegate_task (чистый контекст, глубина ≤1)
│   │   └── browser.go           #   CDP: headless Chromium, текст + скриншоты
│   ├── skills/                 # каталог само-пишущихся навыков (markdown)
│   ├── metrics/                # Prometheus text-эндпоинт (лёгкие счётчики)
│   ├── cron/                   # движок проактивности (every/times + nudge)
│   ├── telegram/               # МОДУЛЬ 3a: Telegram-раннер
│   │   ├── bot.go              #   собственный polling-клиент (anti-ECONNRESET)
│   │   └── runner.go           #   команды, тревоги, сводки, approval-клавиатуры
│   └── ipc/                    # МОДУЛЬ 3b: локальный интерфейс
│       ├── server.go           #   UNIX-сокет, line-JSON, мульти-запросы
│       └── cli.go              #   клиент `archi ctl` (+ тихий режим для ear)
├── deploy/
│   ├── config.yaml             # эталонный конфиг
│   ├── archi.service           # systemd-юнит демона
│   ├── archi-ear.service       # systemd-юнит hands-free слушателя
│   ├── archi-tunnel.service    # systemd-юнит SSH-туннеля к LLM-прокси
│   ├── archi-whisper.service   # локальный whisper.cpp-фолбэк STT
│   └── setup-piper.sh          # установка офлайн-TTS Piper + ru-голосов
├── Makefile
└── README.md
```

## Ресурсы

- RAM в простое: ~30–60 МБ (против 240 МБ MemoryMax у старого Node-бота).
- Цикл телеметрии: раз в 30 с (настраивается), SSH-сессия ~1/цикл из пула.
- Бинарник: ~8 МБ статик (CGO_ENABLED=0), старт < 100 мс.
- Локальный whisper-фолбэк (если поднят archi-whisper.service): ~600 МБ в работе,
  0 в простое при выключенном юните — фолбэк просто не сработает и вернётся ошибка основного провайдера.

## Возможности 2026.14 «Уши»

Релиз про то, что hands-free агент **должен слышать всегда** — без облака, без
ключей, без правильных планет в зените:

- **`archi ear --test`** — самодиагностика слуха одной командой: 8 секунд живого
  индикатора громкости, калибровка шумового пола, VAD-фраза, круговой STT,
  wake-совпадение, доступность IPC-сокета. Вывод человекочитаем — присылай в чат
  для дистанционного разбора.
- **`archi ear --train-wake`** — обучение wake-шаблонов голосом владельца:
  5 образцов «Арчи» → MFCC (c1..c12, 25/10 мс) + DTW со скользящим окном,
  кросс-валидация порога, сохранение в `<data_dir>/wake-templates.json`.
- **Движки wake-word** (`ear.wake_engine`):
  - `stt` — по транскрипту (как в 2026.13; зависит от STT-провайдера);
  - `dtw` — **офлайн**: шаблоны твоего голоса, чистый Go, ноль зависимостей —
    wake работает даже с мёртвым STT/облаком;
  - `http` — сайдкар с моделью openWakeWord (deploy/wake_server.py): натренируй
    «Арчи» за <1 часа на openwakeword.com или локально livekit-wakeword (июнь
    2026, модели совместимы, 0.08 ложных срабатываний/час против 8.5 у классики);
  - `both` — dtw ИЛИ stt.
- **Фиксы глухоты**: parecord теперь уважает `voice.mic.device` (раньше молча
  брал дефолтный источник); авто-выбор бэкенда prefers parecord при живой
  сессии PipeWire; мёртвый поток микрофона перезапускается внутри процесса
  (systemd Restart=always имеет StartLimitBurst и сдаётся навсегда) с stderr
  и подсказками в журнале.
- **Фикс сервиса**: главный источник «не слышит» на Mint 22 — systemd-юнит не
  видит сессию PipeWire. Теперь `archi-ear.service` несёт
  `Environment=XDG_RUNTIME_DIR=/run/user/1000`, а `archi-ear-user.service`
  (юнит пользователя, рекомендуемый путь) стартует прямо в сессии.
- **Толерантный wake-матчинг**: латинская раскладка (`archi`/`archy`), опечатки
  («арче», Левенштейн ≤1), склейки пробелом («ар чи»), пунктуация.

### «Уши не слышат» — чек-лист (по убыванию вероятности)

```bash
systemctl status archi-ear            # 1. слушатель вообще жив?
journalctl -u archi-ear -n 50         # 2. что он говорит? (stderr рекордера, подсказки)
archi ear --test                      # 3. полный прогон цепочки с вердиктами
systemctl --user status archi-ear     # 4. юнит пользователя (если им запускал)
arecord -L | grep -i plughw           # 5. явное устройство → ear.device (voice.mic.device)
```

Топ-3 причины из практики: слушатель не в сессии PipeWire (→ юнит
пользователя), STT-провайдер недоступен (ключ/сеть — см. `--test`, шаг 4),
слишком тихий микрофон (снизь `ear.vad.start_factor`, скажи громче).

## Возможности 2026.13 «Эталон 2026»

GAP-анализ к эталонным агентам 2025–2026 (Hermes, Claude Code, OpenClaw) закрыт
ещё на шесть плоскостей:

- **Hands-free голос** (`internal/voice/ear.go` + `archi ear` + archi-ear.service):
  энергийный RMS-VAD с адаптивным шумовым полом и гистерезисом, pre-roll,
  wake-word «арчи»/«archi» с Левенштейн-допуском опечаток (≤1), полудуплекс
  с barge-in (громкая речь перебивает озвучку), анти-эхо cooldown. Голый
  wake-word активирует следующую фразу («— Арчи. — Да?»). Требует IPC + STT.
- **Само-пишущиеся навыки** (`internal/skills` + `skill_save`/`skill_read`/
  `skill_list`): удачные процедуры агент сохраняет в `memory/skills/*.md`,
  релевантные подмешиваются в системный промпт (поиск по заголовку ×3 /
  тегам ×2 / телу ×1). Паттерн Hermes self-improvement.
- **Субагенты** (`delegate_task`): длинные разведки уходят в чистый контекст
  (своя сессия `sub:<ts>`), вывод возвращается сжатым — диалог не раздувается.
  Вложенное делегирование запрещено (глубина ≤1).
- **Векторный recall памяти**: записи векторизуются OpenAI-совместимым
  /embeddings (вектор хранится прямо в JSONL), скоринг гибридный —
  2.5×косинус + keyword-бонус + свежесть. Без ключей работает как раньше.
- **Браузерный инструмент** (`browser`, `internal/tools/browser.go`):
  single-shot headless Chromium по CDP (собственный WS-клиент) — текст
  JS-отрендеренной страницы + JPEG-скриншоты в `<data_dir>/browser/`.
  Никаких библиотек — только stdlib + x/net/websocket.
- **Observability**: Prometheus-эндпоинт `archi_llm_requests_total`,
  `archi_llm_tokens_total`, `archi_tool_calls_total{tool,result}` и др. на
  127.0.0.1:9109 (config: `observability`).
- **Локальный TTS Piper**: provider `piper` — офлайн-синтез (бинарь +
  ru_RU-голоса: irina/ruslan/dmitri, ставит `deploy/setup-piper.sh`),
  автоконвертация в opus для Telegram sendVoice.
- **LLM-failover**: цепочка `llm.providers` — после исчерпания ретраев
  основного запрос уходит к резервному провайдеру той же формы.

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
- [x] ~~Векторный recall памяти~~ (2026.13, /embeddings + косинус)
- [x] ~~Hands-free «Арчи, …»~~ (2026.13, RMS-VAD + wake-word; Silero/openWakeWord —
      следующий шаг точности, интерфейс VAD готов)
- [x] ~~Локальный TTS Piper ru_RU~~ (2026.13, provider: piper)
- [x] ~~Метрики Prometheus~~ (2026.13, /metrics на localhost)
- [ ] `send_file` / `generate_photo` (Gemini) — перенос из старого бота.
- [ ] Semantic VAD (Silero) + openWakeWord «hey archi» — точность вместо RMS-эмпирики.
- [ ] Дуплекс-режим голоса (WebSocket-стриминг STT/TTS) как Realtime-эталон.
- [ ] Webhook-режим Telegram как альтернатива polling.
