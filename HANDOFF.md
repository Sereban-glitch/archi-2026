# HANDOFF — Голосовой интерфейс Archi 2026 (2026.11)

Обновление добавляет голосовое общение с агентом: голосовые сообщения в Telegram
и локальный микрофон ноутбука (`archi voice`). Сборка остаётся статическим
бинарником без CGO; вся тяжёлая аудио-работа delegated внешним процессам
(ffmpeg/arecord) и HTTP-провайдерам.

---

## 1. Архитектурное решение (итоги разведки 2025–2026)

### STT: OpenAI-совместимый HTTP-протокол вместо библиотек

Рассмотренные варианты и почему выбран именно этот:

| Вариант | Вердикт |
|---|---|
| Go-биндинги `whisper.cpp/bindings/go` | ❌ требуют CGO → рушат `CGO_ENABLED=0` статическую сборку и привносят C++ тулчейн в CI |
| Python-обвязки (faster-whisper, pywhispercpp) | ❌ тяжёлый Python-монолит рядом с Go-бинарником — противоречит легковесности |
| **OpenAI-совместимый `/v1/audio/transcriptions`** | ✅ один клиент — любой провайдер: Groq (`whisper-large-v3-turbo`), OpenAI (`whisper-1`), локальный whisper.cpp server / Speaches / LocalAI |

Выбранный паттерн — **провайдер-агностика через стандартный протокол**:
Groq даёт `whisper-large-v3-turbo` (быстрее и дешевле OpenAI, тот же API),
локальный whisper.cpp поднимается отдельным юнитом `whisper-server` с
OpenAI-совместимым endpoint — код агента не меняется при смене провайдера.

### Telegram voice: OGG/Opus без перекодирования

Telegram Voice — это OGG/Opus, который облачные Whisper-API принимают напрямую
(до 20 МБ, лимит Bot API на скачивание). Перекодирование в WAV 16 кГц через
внешний `ffmpeg` выполняется только для локальных серверов (`convert: auto`
смотрит на host в base_url: localhost → конвертим, облако → шлём оригинал).
Скачивание: `getFile` → `GET /file/bot<token>/<file_path>` с ретраями на
сетевые сбои — тот же класс устойчивости, что у polling.

### Микрофон: arecord + отдельная процессная группа

`archi voice` пишет WAV 16 кГц моно через `arecord` (ALSA; в Mint PipeWire
пробрасывается через pipewire-alsa) с авто-фолбэком на `ffmpeg -f alsa` и
`parecord`. Ключевые детали надёжности:

- рекордер в своей **процессной группе** — Ctrl+C в терминале не убивает его
  мгновенно, а даёт нашему коду послать группе SIGINT → WAV-заголовок
  корректно финализируется (иначе битый файл);
- push-to-talk: Enter — стоп, protect-таймер 120 с обрывает забытую запись;
- пустая/короткая запись (<1500 байт ≈ 47 мс) отбрасывается с понятной ошибкой.

### TTS: опционально, `/audio/speech`

Синтез включается флагом в конфиге; формат `opus` — именно OGG/Opus контейнер,
который Telegram принимает как голосовое (`sendVoice`). Ответы длиннее
`max_chars` не озвучиваются (пользователь получает текст). Локально озвучка
ответа (`--speak`) играется через ffplay → paplay → aplay.

---

## 2. Что изменилось (файлы)

### Новые

| Файл | Назначение |
|---|---|
| `internal/voice/stt.go` | Transcriber: multipart-клиент `/audio/transcriptions`, 3 ретрая (сеть/5xx), лимит размера, авто-конверсия ogg→wav |
| `internal/voice/tts.go` | Synthesizer `/audio/speech` (nil-safe), `Play()` через ffplay/paplay/aplay, `PlainText()` — зачистка markdown перед озвучкой |
| `internal/voice/mic.go` | `RecordInteractive` (push-to-talk), выбор бэкенда arecord/ffmpeg/parecord, сигналы процессной группе |
| `internal/voice/ffmpeg.go` | `ConvertToWAV16k` (внешний ffmpeg), `safeName` (анти-инъекция multipart), `isLocalURL` |
| `internal/voice/voice_test.go` | 8 тестов: мок-провайдеры, RIFF-проверка, авто-конверсия, 4xx, границы |
| `internal/telegram/bot_test.go` | Мок Bot API: getFile+скачивание+sendVoice, нарезка UTF-8, нормализация вложений |

### Изменённые

| Файл | Изменение |
|---|---|
| `internal/config/config.go` | Секция `voice:` (stt/tts/mic) + дефолты + валидация (base_url/model/convert/format/backend) |
| `internal/telegram/bot.go` | `TgVoice`/`TgAudio` в Update; `GetFile`, `DownloadFileBytes` (ретраи), `DownloadAttachment`, `SendVoice` (multipart), `SendChatAction`; `SendMessage` теперь через `pollResponse` → ретраи ECONNRESET/429/409; роутинг voice/audio в `Run` |
| `internal/telegram/runner.go` | `handleVoice`: скачивание → STT → цитата транскрипта + ответ агента; опциональный голосовой ответ TTS; /help упоминает голос |
| `internal/ipc/cli.go` | `RunCLI` возвращает `(string, error)` — ответ нужен для `--speak` |
| `cmd/archi/main.go` | Подкоманда `archi voice [--speak]`; сборка STT/TTS в `serve`; версия 2026.11.0 |
| `deploy/config.yaml` | Секция `voice:` с комментариями (env-подстановка `${GROQ_API_KEY:-}`) |

### Исправленные попутно дефекты

1. **SendMessage терял сообщения** при ECONNRESET/429 — раньше одна попытка на
   чанк; теперь полный retry-контур как у getUpdates.
2. **runCtl не умел** передавать ответ вызывающему —signature расширена для TTS.
3. **Проверка file_path** перед скачиванием (защита от `..` и инъекций схемы).

---

## 3. Поток данных

```
Telegram Voice (OGG/Opus ≤20МБ)
  └→ bot.getFile + GET /file/bot… (ретраи) → байты
      └→ Transcriber: облако? ── да ──→ multipart → /audio/transcriptions
                    └─ нет (localhost) → ffmpeg → WAV16k → multipart
          └→ {"text": …}
              └→ agent.Handle (ReAct, те же инструменты/сессия tg:<chat>)
                  └→ SendMessage ("🎙 «транскрипт» + ответ")
                  └→ [TTS вкл.] /audio/speech → opus → bot.SendVoice

archi voice [--speak]
  └→ arecord/ffmpeg/parecord (pgid, Enter=стоп, ≤120с) → WAV16k
      └→ Transcriber → транскрипт → IPC-сокет демона → ответ
          └→ stdout; [ --speak ] TTS → ffplay/paplay/aplay
```

---

## 4. Проверка на ноутбуке (пошагово)

```bash
# 0) зависимости (обычно уже есть в Mint)
sudo apt install -y alsa-utils ffmpeg pulseaudio-utils

# 1) сборка и статический анализ
go vet ./... && go test ./...          # 11 тестов, все PASS
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/archi ./cmd/archi

# 2) конфиг: включи STT (Groq — самый быстрый старт)
#    export GROQ_API_KEY=gsk_...      ← https://console.groq.com
sed -i 's/^    enabled: false              # включи после настройки ключа/    enabled: true/' deploy/config.yaml
archi check-config --config deploy/config.yaml   # → OK

# 3) Telegram: перезапусти демон и отправь боту голосовое сообщение
sudo systemctl restart archi
journalctl -u archi -f | grep voice    # → "voice: STT включён"

# 4) Локальный микрофон
archi voice --config deploy/config.yaml            # текстовый ответ
archi voice --speak --config deploy/config.yaml    # + озвучка (нужен voice.tts.enabled)

# 5) Локальный whisper.cpp вместо облака (опционально, приватность)
#    ~/whisper.cpp/server -m models/ggml-base-q5_1.bin --port 9000 --host 127.0.0.1
#    в config.yaml: voice.stt.base_url: http://127.0.0.1:9000/v1, api_key: ""
#    convert: auto сам переключится на ogg→wav конверсию для localhost
```

Ожидаемое поведение при сбоях: недоступен STT-провайдер → в чат придёт
«⚠️ Распознавание не удалось: …» (3 ретрая с backoff), сообщение не теряется;
пустая транскрипция → подсказка перезаписать; голосовое при выключенном STT →
«🎙 Голосовые сообщения отключены в конфиге».

## 5. Ограничения и roadmap

- Whisper не видит форматирование: транскрипт идёт в агент как чистый текст —
  это нормально для ReAct-запросов.
- TTS-провайдеры: OpenAI `gpt-4o-mini-tts` / `tts-1`, Groq `playai-tts`
  (голоса вида `Fritz-PlayAI`); локальный Piper — через LocalAI-совместимый шлюз.
- push-to-talk, а не VAD: осознанный выбор для CLI (простота, предсказуемость);
  roadmap — горячая клавиша и непрерывный VAD-режим.
- Голосовые ответы Telegram шлются только на короткие ответы (≤ max_chars).
