# HANDOFF — Archi 2026.15 «Живой голос»

Релиз по пользовательскому фидбеку на озвучку 2026.14: дребезг Piper, чтение
знаков препинания и чисел буквально, админские простыни в голос и задержка.
Ниже — что добавлено к 2026.14; далее прежний handoff без изменений.

---

## 0. Что в 2026.15 (кратко)

| Модуль | Где | Что делает |
|---|---|---|
| Нормализация речи | `internal/voice/normalize.go` | «2.56»→«2 и 56», GiB→«гигабайт», %→«процентов», markdown/эмодзи/URL вычищаются; фразы для стриминга `SpeechChunks` |
| Параметры Piper | `voice/tts.go` (`piperTuningArgs`), `config.go` | noise_scale 0.333 / noise_w 0.6 / length_scale 0.95 — дефолты, против дребезга и дрожи |
| Потоковая озвучка | `voice/tts.go` (`Speak`) | piper `--output_raw` → плеер напрямую (ffplay/pacat/aplay): первое предложение звучит до готовности всего текста; частота из .onnx.json |
| Разговорный режим ear | `agent/prompt.go` (`VoiceStyleGuide`), `ipc` (флаг `voice`), `ear.go` | агент отвечает 1–2 предложения живым языком без markdown; сессия `ear` отделена от `cli` |
| Провайдер command | `voice/tts.go` (`synthesizeCommand`) | любой внешний TTS: текст→stdin, аудио→stdout; готовая обёртка Edge-TTS `deploy/edge-tts.sh` |
| Тайминги ear | `ear.go` | журнал: `stt_ms / llm_ms / total_ms` — видно, где живут секунды |

### 0.1. Разговорный режим — как включён

`archi ear` шлёт IPC-запрос с `"voice":true` → сервер маршрутизирует в сессию
`ear` и зовёт `agent.HandleOpts(..., Voice: true)` → к системному промпту
дописывается «РЕЖИМ УШЕЙ» (кратко, разговорно, числа словами, без разметки).
Протокол обратно совместим: старые клиенты без поля `voice` работают как раньше.

### 0.2. Латентность ear (замер после релиза)

```
тишина 800ms → STT ~400ms → LLM ~0.8s (короткий голосовой ответ)
→ piper первая фраза ~150ms → ЗВУК; итого ≈1.4–1.8s (в 2026.14 было ~2.8s)
```

### 0.3. Смена голоса/движка (по фидбеку «голос неживой»)

```bash
sudo bash deploy/setup-piper.sh denis     # четвёртый русский голос piper
# параметры живости уже в config.yaml (noise_scale 0.333 / noise_w 0.6 / length_scale 0.95)
# если piper не нравится совсем — облачный Edge-TTS (естественнее всех):
sudo pip install --upgrade edge-tts
sudo install -m 755 deploy/edge-tts.sh /usr/local/bin/edge-tts-archi
# в конфиге: voice.tts.provider: command, voice.tts.format: mp3,
#            voice.tts.command.bin: /usr/local/bin/edge-tts-archi
```

### 0.4. Прочие движки (разведка октябрь 2026)

- **Silero v5** — лучший офлайн-русский, CC BY-NC-SA (личное ок), тот же контракт command;
- **piper1-gpl** (OHF) — живой форк piper, бинарная замена;
- **Kokoro-82M** — русского нет; **edge-tts** — жив, требует свежих версий пакета.

---

## 1. Прежний handoff — 2026.14 «Уши» (без изменений)

Релиз про офлайн-wake-word и фикс «не слышит»: MFCC+DTW-движок (`archi ear
--train-wake`), диагностика `archi ear --test`, движки stt|dtw|http|both,
фиксы PipeWire-глухоты сервисом (XDG_RUNTIME_DIR, юнит пользователя).

---

# HANDOFF — Archi 2026.13 «Эталон 2026»

Turnkey-релиз: все фиксы аудита + все модули, закрывающие GAP к эталонным
агентам 2025–2026. Ничего включать в коде не надо — только собрать, положить
конфиг и запустить юниты. Ниже — что добавлено к 2026.12, затем прежний
handoff 2026.12 без изменений.

---

## 0. Что в 2026.13 (кратко)

| Модуль | Где | Что делает |
|---|---|---|
| Hands-free голос | `internal/voice/ear.go`, `archi ear`, `deploy/archi-ear.service` | RMS-VAD + wake-word «арчи» → STT → агент → TTS; barge-in |
| Само-пишущиеся навыки | `internal/skills`, `tools/skills.go` | skill_save/read/list, инъекция в промпт |
| Субагенты | `tools/subagent.go` | delegate_task: чистый контекст, глубина ≤1 |
| Векторный recall | `agent/embed.go`, `memory.go` | /embeddings + косинус + keyword-гибрид |
| Браузер | `tools/browser.go` | headless Chromium по CDP: текст + скриншоты |
| Observability | `internal/metrics` | Prometheus /metrics + /healthz, 127.0.0.1:9109 |
| TTS Piper | `voice/tts.go`, `deploy/setup-piper.sh` | офлайн-озвучка, ru-голоса, автоконвертация opus |
| LLM-failover | `agent/llm.go` | цепочка `llm.providers` после ретраев основного |

### 0.1. Hands-free `archi ear` — как устроен

- **Отдельный процесс** (systemd): микрофонный цикл не умирает при рестарте
  демона; команда уходит через IPC-сокет (`ipc.RunCLIQuiet`).
- **VAD**: энергия RMS, адаптивный шумовой пол (калибровка `noise_learn_s`,
  затем медленная EMA в тишине), гистерезис start_factor/stop_factor,
  `min_speech_ms` отфильтровывает клики, pre-roll ~400 мс не режет начало
  wake-word, `max_utterance` обрубает бесконечную речь.
- **Wake-word**: первый токен транскрипта после нормализации (ё→е, пунктуация);
  допуск опечаток Левенштейн ≤1 для токенов ≥4 символов («арчии» сработает).
  Голый wake-word → режим «слушаю следующую фразу».
- **Полудуплекс + barge-in**: во время озвучки кадры НЕ кормят VAD (эхо не
  портит шумовой пол), но громкая устойчивая речь (×1.8 от старта, ~240 мс)
  отменяет плеер. После ответа — cooldown (анти-эхо).
- Валидация: `ear.enabled=true` требует `ipc.enabled=true` и
  `voice.stt.enabled=true` (проверяет `check-config`).
- Запуск: `sudo systemctl enable --now archi-ear`, логи `journalctl -u archi-ear -f`.

### 0.2. Навыки и субагенты

- Навык = markdown `memory/skills/<name>.md` («# Заголовок», «tags: …», тело).
  Поиск: заголовок ×3 / теги ×2 / тело ×1; топ-`inject_limit` подмешивается
  в системный промпт на каждый запрос. Сохранение атомарно (tmp+rename).
- `delegate_task`: субагент получает ту же панель инструментов, но свою
  сессию `sub:<unixts>`; вывод урезается до 8 КБ; вложенность запрещена
  (глобальный CAS-счётчик глубины).
- Выключаются: `skills.enabled: false`; субагент-инструмент не регистрируется,
  пока `SetSubAgent` не подключён (в main подключается всегда после старта).

### 0.3. Память с эмбеддингами

- `memory.embeddings.enabled: true` → каждая запись векторизуется (батч
  на Save, таймаут 15 с; неудача не блокирует запись — остаётся keyword-recall).
- Recall: score = keyword-насыщение + 2.5×косинус(query, entry.vec) +
  свежесть ×1.2 (<7 дней). Вектор живёт прямо в JSONL — компакция его
  сохраняет автоматически.

### 0.4. Браузер

- `browser.enabled: true` + установленный chromium. Single-shot: чистый
  профиль на вызов, DevTools WS, `Page.navigate` → load → `Runtime.evaluate`
  (innerText) → опционально `Page.captureScreenshot` (JPEG, q70) в
  `<data_dir>/browser/`. Процесс гасится всегда (defer kill).
- НЕ для интерактивных потоков «логин → клик → 2FA» — это остаётся за
  владельцем (принцип Operator takeover).

### 0.5. Observability

- `observability.metrics_addr: 127.0.0.1:9109` → `GET /metrics` (Prometheus
  text: `archi_llm_requests_total`, `archi_llm_tokens_total{dir=in|out}`,
  `archi_tool_calls_total{tool,result}`, `archi_uptime_seconds`) и
  `GET /healthz`. Выключается `observability.enabled: false`.
- Ноль зависимостей: собственный счётчикный реестр с мьютексом.

### 0.6. TTS Piper (офлайн)

- `sudo bash deploy/setup-piper.sh [irina|ruslan|dmitri]` → /opt/piper.
- Конфиг: `voice.tts.provider: piper`, `piper.bin`, `piper.model`.
- Синтез: текст → stdin бинаря → WAV → (ffmpeg) → opus для Telegram.
  Без ffmpeg отдаёт WAV (локальное воспроизведение работает).

### 0.7. LLM-failover

- `llm.providers: [{base_url, api_key, model}]` — обходится по порядку,
  когда основной (включая свои 5 ретраев) исчерпан. Форма запросов та же
  Anthropic Messages — подойдёт любой прокси/зеркало.

### 0.8. Включение на ноутбуке (дополнение к §4)

```bash
git pull && go vet ./... && go test ./... && make build
sudo cp bin/archi /usr/local/bin/ && sudo systemctl restart archi

# голос полный стек:
sudo bash deploy/setup-piper.sh               # офлайн-TTS (опционально)
# config.yaml: voice.stt.enabled: true, voice.tts.provider: piper|openai,
#              ear.enabled: true
sudo cp deploy/archi-ear.service /etc/systemd/system/ && systemctl daemon-reload
sudo systemctl enable --now archi-ear
sudo systemctl restart archi
```

### 0.9. Как проверялось (2026.13)

- `go vet` чист; `go test ./...` — 30 тестов в 7 пакетах, все PASS
  (новые: VAD-фраза/клик/лимит, wake-word с опечатками, editDistance,
  WAV-заголовок, playExt; skills, metrics, memory-эмбеддинги).
- E2E-смоук: serve → IPC-сокет → `ctl --status` (usage + ноды) →
  `curl /metrics` + `/healthz` → graceful-фолбэк без LLM → `archi ear`
  корректно отказывает при `ear.enabled=false`.
- Пойман и исправлен дедлок `SetSubAgent` (двойной захват r.mu) — нашёлся
  именно смоук-запуском: демон молча висел до IPC. В 2026.12 его не было,
  т.к. SetSubAgent тогда не вызывался из main.
- `check-config` на deploy/config.yaml — ОК (включая новые секции).
- Статик CGO_ENABLED=0, 8.8 МБ, `archi version` → 2026.13.0.

---

# Прежний HANDOFF — Archi 2026.12 «Автономность»

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
