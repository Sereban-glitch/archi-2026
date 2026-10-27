# HANDOFF — Archi 2026.12 «Автономность»

Релиз закрывает топ-3 разрыва с эталонными агентами 2026 (аудит/разведка,
см. §1) и всю критическую findings-часть аудита кода: **проактивность**
(cron-задания), **интерактивные approval-клавиатуры** в Telegram,
**голосовой фолбэк** на локальный whisper.cpp, плюс 4 P1-фикса и россыпь
P2/P3. Сборка — по-прежнему статический бинарник без CGO (8.2 МБ).

---

## 1. Почему эти три модуля (выжимка GAP-анализа)

Разведка OpenAI Operator/Realtime, Hermes Agent v0.22, OpenClaw 2.0,
Claude Code, Pi/Goose/Amp/Codex показала 9 плоскостей отставания Archi.
Топ-3 по соотношению «влияние на автономность / стоимость закрытия»:

1. **Проактивность** — Hermes cron (first-class agent tasks + failure-nudge ×3),
   OpenClaw GatewayScheduler, Amp automations. Агент без расписания —
   реактивный чат-бот: владелец остаётся единственным источником инициативы.
2. **Approval перед side-effects** — Operator (подтверждение действий),
   Claude Code (permission modes), Hermes (approval.py + write_approval
   для памяти). Deny-regex один не масштабируется: он решето
   (`rm -rf /home` проходил).
3. **Observability** — счётчики токенов/запросов: без них LLM-агент
   невозможно осознанно удешевлять.

## 2. Что нового по модулям

### 2.1. Cron-движок (`internal/cron`) — проактивность

- Расписание в config.yaml: `every: 6h` ИЛИ `times: ["09:05"]` (взаимоисключающи,
  валидатор ловит нарушение; `every` < 1m запрещён).
- Каждый запуск — полный ReAct-цикл агента в собственной сессии `cron:<name>`
  (TTL и компактизация — как у всех сессий).
- **Failure-nudge** как у Hermes: до 3 попыток с backoff 5m; после третьей —
  алерт админам. Демон не останавливается.
- Результат: админам в Telegram (`notify: true`) + episode в долговременную
  память (_recall работает в будущих сессиях).
- `run_on_start: true` — разовый прогон через минуту после старта (ноутбук
  мог проспать слот).
- `NextSlot()` смотрит слоты сегодня И завтра — тот же класс бага, что был
  в digestLoop (P1-1), здесь исключён конструционно (тест `TestNextSlotDaily`).
- Telegram выключен → результаты в лог (logNotifier), движок работает.

### 2.2. Approval-клавиатуры (`tools/approval.go` + telegram) — доверие

Трёхуровневая политика bash-инструментов, порядок строгий:

1. **deny-паттерны** (жёсткий отказ, без вопросов) — усилены аудитом:
   `--no-preserve-root`, `find / … -delete`, `curl|sh`, `wget|bash`.
2. **approval-паттерны** (спросить владельца) — рестарты systemd, `rm -r/-f`,
   `pkill/killall/kill`, chmod/chown в `/etc`, iptables/ufw, `drop table/database`,
   `git push`. Список настраивается, паттерны прекомпилированы.
3. Остальное исполняется как раньше.

Механика: `Registry.confirm()` → `Runner.Ask()` → inline-клавиатура
«✅ Разрешить / ❌ Отклонить» первому админу. Кнопка → callback_query →
канал решения → ответ LLM («ОТКЛОНЕНО ВЛАДЕЛЬЦЕМ…» либо исполнение).
Клавиатура после решения снимается (editMessageText). **Fail-closed**:
нет ответа за `approval_timeout` (2m) / Telegram выключен / канал умер →
отказ. Одобренные команды пишутся в память (episode) — след аудита.
Bot API: `allowed_updates` теперь `["message","callback_query"]`, добавлены
`SendKeyboard/AnswerCallback/EditMessageText` (все через pollResponse —
с теми же ECONNRESET-ретраями).

### 2.3. Голосовой фолбэк (`voice/stt.go` + `deploy/archi-whisper.service`)

Эталонная схема звука на HP ProBook 440 G3 (i5-6200U 2C/4T, 12 ГБ):

- **Первичный** — Groq `whisper-large-v3-turbo` (~0.4-1.2 с, WER ru ~6-8%,
  $0.04/час; free tier 1K req/день покрывает личное использование).
- **Фолбэк** — локальный whisper.cpp server, модель `ggml-small-q5_0`
  (RTF ~0.3-0.5 на этом CPU, ~600 МБ RAM, WER ru ~11-14%): поднимается
  юнитом `archi-whisper.service` (CPUWeight=25, Nice=10, MemoryMax=1500M),
  переключение автоматическое при недоступности основного (сеть/429/ключ).
  Фолбэк сам решает конверсию OGG→WAV16k (convert: auto + localhost).
- **voice_typer.service владельца НЕ трогаем**: его левые/правые Ctrl+Alt —
  диктовка в сфокусированное окно; Archi — командование агентом
  (Telegram-голос / `archi voice`). Разные роли, ноль конфликтов.

### 2.4. P1-фиксы аудита (все 4)

| # | Дефект | Файл | Фикс |
|---|--------|------|------|
| P1-1 | digestLoop пропускал завтрашний слот | telegram/runner.go | слоты ищутся сегодня+завтра |
| P1-2 | 429/5xx не ретраились (мёртвая ветка) | agent/llm.go | `apiError.Status`; 429 и ≥500 ретраебельны, Retry-After работает |
| P1-3 | data race на `SSHPool.fails` | bridge/ssh.go | чтение под локом |
| P1-4 | `history_limit` не подключён | agent/session.go, agent.go | лимит сообщений истории в сессии |

### 2.5. P2/P3-фиксы (выборка)

- **TOCTOU compact()**: ReAct-циклы одной сессии сериализуются
  (`Session.HandleMu`) — Telegram шлёт апдейты параллельными горутинами,
  раньше compact мог потерять сообщения (тестами подтверждён класс бага).
- **Eviction mid-run**: сессия с идущим циклом не вытесняется (busy-флаг).
- **IPC-сессии**: все коннекты UNIX-сокета коллизировали в `"ipc:@"` —
  теперь явная сессия `"cli"` (контекст `archi ctl` сохраняется).
- **UTF-8**: усечения вывода инструментов и компактизации больше не рвут
  руны (прямой скан границ; тест).
- **Память**: memory.jsonl компактируется при переполнении (гистерезис ×1.2,
  атомарный tmp+rename+fsync) — файл не растёт вечно, Recall видит всё окно.
- **Deny-решето**: см. §2.2; **симлинки** в writeAllowed раскрываются
  (EvalSymlinks) — `~/ln → /etc` не обманет префиксную проверку.
- **forbidden_read** паттерны прекомпилированы (перекомпиляция на каждый
  read_file убрана).
- `stop_reason == "max_tokens"` → честное предупреждение в ответе.
- TTS-озвучка использует родительский ctx (actx мог быть исчерпан ReAct-циклом).
- Usage-счётчики: `/status` в Telegram и `archi ctl --status` показывают
  запросы + токены in/out с старта демона.
- Мёртвый код убран (поле `hints`, `&& false` в isRetryableNet).

## 3. Конфигурация (deploy/config.yaml)

Новые секции: `tools.approval_patterns` + `tools.approval_timeout`,
`voice.stt.fallback` (полная форма провайдера), `cron.jobs[]`
(3 готовых промпта: утренняя проверка, гигиена диска, вечерний обзор памяти).
`cron.enabled: false` по умолчанию — включай после ревизии промптов.

## 4. Установка на ноутбук (полный путь)

```bash
git clone https://github.com/Sereban-glitch/archi-2026.git && cd archi-2026
go vet ./... && go test ./...          # должны быть зелёными
make build                             # bin/archi, CGO_ENABLED=0

sudo useradd -r -m archi 2>/dev/null || true
sudo mkdir -p /etc/archi /var/lib/archi
sudo cp deploy/config.yaml /etc/archi/
sudo cp bin/archi /usr/local/bin/
sudo cp deploy/archi.service deploy/archi-tunnel.service /etc/systemd/system/
sudo systemctl daemon-reload

# секрета нет в git — задай окружение:
sudo systemctl edit archi   # Environment=TELEGRAM_BOT_TOKEN=… GROQ_API_KEY=…

# включить голос (фаза 0): /etc/archi/config.yaml → voice.stt.enabled: true
sudo systemctl enable --now archi archi-tunnel

# (опционально, фаза 1) локальный фолбэк STT:
#   инструкции в начале deploy/archi-whisper.service (сборка whisper.cpp,
#   модель ggml-small-q5_0 184 МБ, юнит)
```

Проверка: `archi ctl --status`, `/status` в Telegram, голосовое сообщение
боту, «перезапусти console-bot» → клавиатура ✅/❌.

## 5. Совместимость и миграция

- Конфиг 2026.11 совместим на 100%: новые секции имеют дефолты, старые
  ключи не переименовывались. `check-config` валидирует всё новое.
- `Bot.Run` сменил сигнатуру (добавлен callback-хендлер) — это внутренний
  API, для внешних интеграций всё стабильно (IPC-протокол не менялся).
- База памяти, сокет, пути — без изменений.

## 6. Как проверялось

- `go vet` чист; `go test ./...` — 19 тестов в 5 пакетах, все PASS
  (новые: cron NextSlot ×3, deny-усиление ×2 набора, approval-паттерны ×2
  набора, UTF-8 truncate ×2).
- `check-config` — позитив (deploy-конфиг) и негатив (every+times вместе
  ловится валидатором).
- Статическая сборка CGO_ENABLED=0, 8.2 МБ, `archi version` → 2026.12.0.
