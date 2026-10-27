#!/usr/bin/env bash
# ============================================================
# setup-piper.sh — установка локального офлайн-синтеза речи Piper
# для Archi 2026 (ноутбук HP ProBook, Linux Mint 22).
#
# Скачивает бинарь piper (rhasspy/piper) и русский голос .onnx,
# кладёт всё в /opt/piper и печатит готовый кусок для config.yaml.
#
# Использование:
#   sudo bash deploy/setup-piper.sh          — голос irina (medium, ~63 МБ)
#   sudo bash deploy/setup-piper.sh ruslan   — мужской голос (medium)
#   sudo bash deploy/setup-piper.sh dmitri   — мужской голос (medium)
#   sudo bash deploy/setup-piper.sh denis    — мужской голос (medium)
#
# «Живой голос» (2026.15): против дрожи/дребезга piper в конфиге задаются
#   noise_scale: 0.333, noise_w: 0.6, length_scale: 0.95 (см. deploy/config.yaml).
#
# После установки в конфиге:
#   voice:
#     tts:
#       enabled: true
#       provider: piper
#       format: opus
#       piper:
#         bin: /opt/piper/piper
#         model: /opt/piper/ru_RU-irina-medium.onnx
# ============================================================
set -euo pipefail

VOICE="${1:-irina}"
DEST="/opt/piper"
GH="https://github.com/rhasspy/piper/releases"
HF="https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0"

# Бинарь: последний стабильный релиз под amd64.
BIN_URL="$GH/latest/download/piper_linux_x86_64.tar.gz"
MODEL_URL="$HF/ru/ru_RU-$VOICE-medium/1.0/ru_RU-$VOICE-medium.onnx"
MODEL_JSON_URL="$HF/ru/ru_RU-$VOICE-medium/1.0/ru_RU-$VOICE-medium.onnx.json"

case "$VOICE" in
  irina|ruslan|dmitri|denis) ;;
  *) echo "Неизвестный голос: $VOICE (доступны: irina, ruslan, dmitri, denis)"; exit 1 ;;
esac

mkdir -p "$DEST"
echo ">>> Качаю piper: $BIN_URL"
curl -fSL --retry 3 -o /tmp/piper.tar.gz "$BIN_URL"
tar -xzf /tmp/piper.tar.gz -C /tmp
# в архиве piper/piper — кладём как $DEST/piper
install -m 755 /tmp/piper/piper "$DEST/piper"
rm -rf /tmp/piper /tmp/piper.tar.gz

echo ">>> Качаю голос ru_RU-$VOICE-medium…"
curl -fSL --retry 3 -o "$DEST/ru_RU-$VOICE-medium.onnx" "$MODEL_URL"
curl -fSL --retry 3 -o "$DEST/ru_RU-$VOICE-medium.onnx.json" "$MODEL_JSON_URL"

echo ">>> Проверка синтеза…"
echo "Проверка голоса" | "$DEST/piper" \
  --model "$DEST/ru_RU-$VOICE-medium.onnx" \
  --noise_scale 0.333 --noise_w 0.6 --length_scale 0.95 \
  --output_file /tmp/piper-test.wav
rm -f /tmp/piper-test.wav

cat <<EOF

>>> Готово. Проверь в конфиге /etc/archi/config.yaml:

voice:
  tts:
    enabled: true
    provider: piper          # вместо openai — локальный синтез
    format: opus             # для Telegram sendVoice
    max_chars: 600           # длинные ответы — текстом
    piper:
      bin: $DEST/piper
      model: $DEST/ru_RU-$VOICE-medium.onnx
      timeout: 30s
      noise_scale: 0.333      # «Живой голос»: ровнее, без дребезга
      noise_w: 0.6            # без дрожи на согласных
      length_scale: 0.95      # бодрый темп

>>> Голос сменить: sudo bash deploy/setup-piper.sh denis
>>> Не нравится piper вообще? — provider: command + deploy/edge-tts.sh
    (облачный Edge-TTS Дмитрия: естественнее, нужен интернет)
EOF
