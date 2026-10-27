#!/usr/bin/env python3
"""wake_server.py — сайдкар wake-word для `archi ear` (ear.wake_engine: http).

Протокол: POST /wake, тело — WAV (16 кГц, моно, s16); ответ — JSON:
    {"detected": bool, "score": 0.0..1.0, "keyword": "..."}

Модели (формат openWakeWord, ONNX/TFLite, <100 КБ, realtime на CPU):
  1) ГОТОВАЯ «Арчи» — натренируй за <1 часа на https://openwakeword.com
     (любой язык, облако их GPU, Apache 2.0 на выходе; готовый .onnx скачай).
  2) ЛОКАЛЬНО одной командой — livekit-wakeword (июнь 2026, open-source):
     pip install livekit-wakeword && lw train --phrase "арчи" --out archi.onnx
     (модели совместимы с openWakeWord-пайплайном).
  3) ГОТОВЫЕ англ. — hey_jarvis, alexa, hey_mycroft... (если не задан WAKE_MODELS).

Зависимости: pip install openwakeword onnxruntime
Запуск:      WAKE_MODELS=/opt/wake/арчи.onnx python3 wake_server.py [порт]
             (порт по умолчанию 8079; archi: ear.wake_server_url: http://127.0.0.1:8079/wake)

На i5-6200U инференс openWakeWord занимает проценты одного ядра.
"""
import io
import json
import os
import sys
import wave
from http.server import BaseHTTPRequestHandler, HTTPServer

try:
    from openwakeword.model import Model
except ImportError:
    print("pip install openwakeword onnxruntime", file=sys.stderr)
    sys.exit(1)

MODEL_PATHS = [p.strip() for p in os.environ.get("WAKE_MODELS", "").split(",") if p.strip()]
THRESHOLD = float(os.environ.get("WAKE_THRESHOLD", "0.5"))

print(f"wake_server: модели={MODEL_PATHS or 'встроенные'}, порог={THRESHOLD}")
model = Model(wakeword_models=MODEL_PATHS or None, inference_framework="onnx")


def read_wav(data: bytes):
    with wave.open(io.BytesIO(data)) as w:
        assert w.getframerate() == 16000 and w.getnchannels() == 1, "нужен WAV 16 кГц моно"
        return w.readframes(w.getnframes())


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/wake":
            self.send_error(404)
            return
        n = int(self.headers.get("Content-Length", 0))
        try:
            pcm = read_wav(self.rfile.read(n))
        except Exception as e:  # noqa: BLE001
            self.send_error(400, f"плохой WAV: {e}")
            return
        scores = model.predict(pcm)  # dict {имя_модели: скор}
        keyword, best = max(scores.items(), key=lambda kv: kv[1]) if scores else ("", 0.0)
        detected = best >= THRESHOLD
        body = json.dumps({
            "detected": bool(detected),
            "score": round(float(best), 3),
            "keyword": keyword,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):  # тише в журнал
        print(f"wake_server: {fmt % args}", file=sys.stderr)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8079
    print(f"wake_server: слушаю http://127.0.0.1:{port}/wake")
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
