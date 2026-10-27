#!/usr/bin/env bash
# ============================================================
# edge-tts.sh — обёртка облачного Microsoft Edge-TTS для Archi
# (провайдер voice.tts.provider: command, релиз 2026.15 «Живой голос»).
#
# Контракт command-провайдера: текст приходит на STDIN,
# аудио уходит на STDOUT в формате voice.tts.format (по умолчанию mp3
# — Edge-TTS отдаёт mp3-поток, ffmpeg-конвертация в opus для Telegram
# происходит автоматически).
#
# Установка:
#   sudo pip install --upgrade edge-tts     # или: pipx install edge-tts
#   sudo install -m 755 deploy/edge-tts.sh /usr/local/bin/edge-tts-archi
#
# Конфиг /etc/archi/config.yaml:
#   voice:
#     tts:
#       enabled: true
#       provider: command
#       format: mp3
#       max_chars: 600
#       command:
#         bin: /usr/local/bin/edge-tts-archi
#         args: []
#         timeout: 60s
#
# Голоса (русские): ru-RU-DmitryNeural (м), ru-RU-SvetlanaNeural (ж),
#   ru-RU-PavelNeural? — полный список: edge-tts --list-voices | grep ru-RU
# Смена голоса: Environment=EDGE_TTS_VOICE=ru-RU-DmitryNeural в юните
#   или EDGE_TTS_VOICE=... при запуске демона.
#
# ВАЖНО (разведка 2026): Microsoft периодически вращает токены DRM —
# библиотека держит протокол свежим, но требует `pip install --upgrade
# edge-tts` при ошибках 403. Облачный сервис бесплатен для личного
# использования, но это НЕ официальный API — для продакшена берите
# piper (офлайн) или Silero (локально, лицензия CC BY-NC-SA).
# ============================================================
set -euo pipefail

VOICE="${EDGE_TTS_VOICE:-ru-RU-DmitryNeural}"
RATE="${EDGE_TTS_RATE:-+0%}"       # напр. +10% — бодрее

TEXT="$(cat)"

exec edge-tts \
  --voice "$VOICE" \
  --rate "$RATE" \
  --text "$TEXT" \
  --write-media /dev/stdout
