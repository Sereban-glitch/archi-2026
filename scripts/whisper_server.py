#!/usr/bin/env python3
"""
Lightweight OpenAI-compatible Whisper HTTP server for Archi 2026.
Listens on 127.0.0.1:9000 and serves /audio/transcriptions and /v1/audio/transcriptions.
Uses faster-whisper with cached 'small' model on CPU (int8, 4 threads).
"""

import io
import json
import logging
import os
import sys
import tempfile
from http.server import HTTPServer, BaseHTTPRequestHandler
import cgi

from faster_whisper import WhisperModel

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

HOST = "127.0.0.1"
PORT = 9000
MODEL_SIZE = os.environ.get("WHISPER_MODEL", "small")

logging.info(f"Loading faster-whisper model '{MODEL_SIZE}' on CPU (int8, offline)...")
model = WhisperModel(MODEL_SIZE, device="cpu", compute_type="int8", cpu_threads=4, local_files_only=True)
logging.info(f"Model '{MODEL_SIZE}' ready on {HOST}:{PORT}")


class WhisperHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path in ("/", "/health", "/v1/health"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok","model":"' + MODEL_SIZE.encode() + b'"}')
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        path = self.path.split("?")[0]
        if path not in ("/audio/transcriptions", "/v1/audio/transcriptions"):
            self.send_response(404)
            self.end_headers()
            return

        try:
            content_type = self.headers.get("Content-Type", "")
            if "multipart/form-data" not in content_type:
                # Handle raw binary payload
                length = int(self.headers.get("Content-Length", 0))
                audio_data = self.rfile.read(length)
                suffix = ".wav"
            else:
                # Parse multipart
                form = cgi.FieldStorage(
                    fp=self.rfile,
                    headers=self.headers,
                    environ={
                        "REQUEST_METHOD": "POST",
                        "CONTENT_TYPE": content_type,
                    },
                )
                file_item = form["file"] if "file" in form else None
                if file_item is None:
                    self.send_response(400)
                    self.end_headers()
                    self.wfile.write(b'{"error":"file field missing"}')
                    return

                audio_data = file_item.file.read()
                filename = getattr(file_item, "filename", "") or "audio.wav"
                suffix = os.path.splitext(filename)[1] or ".wav"

            if not audio_data:
                self.send_response(400)
                self.end_headers()
                self.wfile.write(b'{"error":"empty audio data"}')
                return

            with tempfile.NamedTemporaryFile(delete=False, suffix=suffix) as tmp:
                tmp.write(audio_data)
                tmp_path = tmp.name

            try:
                segments, info = model.transcribe(
                    tmp_path,
                    language="ru",
                    vad_filter=True,
                    beam_size=1,
                    condition_on_previous_text=False,
                )
                text = " ".join(s.text.strip() for s in segments).strip()
            finally:
                if os.path.exists(tmp_path):
                    os.unlink(tmp_path)

            logging.info(f"Transcribed ({len(audio_data)} bytes) -> {text[:60]}...")

            response_data = json.dumps({"text": text}, ensure_ascii=False).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(response_data)))
            self.end_headers()
            self.wfile.write(response_data)

        except Exception as e:
            logging.exception("Transcription error")
            err_msg = json.dumps({"error": str(e)}).encode("utf-8")
            self.send_response(500)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(err_msg)

    def log_message(self, format, *args):
        # Suppress verbose default access logs, keep clean
        pass


def main():
    server = HTTPServer((HOST, PORT), WhisperHandler)
    logging.info(f"Serving Whisper STT on http://{HOST}:{PORT}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
