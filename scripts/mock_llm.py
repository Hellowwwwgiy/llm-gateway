"""
Mock LLM Server — 模拟 OpenAI 兼容端点，用于本地验证 SmartProxy 全链路。
同时支持非流式和 SSE 流式响应。

启动: python mock_llm.py  (默认监听 :9999)

SmartProxy 指向它:
  OPENAI_API_KEY=any
  OPENAI_BASE_URL=http://localhost:9999
  OPENAI_MODEL=mock-gpt

请求:
  POST http://localhost:9999/chat/completions
  {"model":"mock-gpt","messages":[{"role":"user","content":"hi"}],"stream":false}
  {"model":"mock-gpt","messages":[{"role":"user","content":"hi"}],"stream":true}
"""
import json
import time
import random
from http.server import HTTPServer, BaseHTTPRequestHandler

HOST = "0.0.0.0"
PORT = 9999
SSE_CHARS_PER_CHUNK = 1
TOTAL_RESPONSE = "Hello! I'm the mock LLM, everything works fine. 🏗️ SmartProxy is awesome."


class MockHandler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        print(f"[mock-llm] {args[0]}")

    def do_POST(self):
        if self.path != "/chat/completions":
            self.send_error(404, f"unknown path: {self.path}")
            return

        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        try:
            req = json.loads(body)
        except Exception:
            self.send_error(400, "bad json")
            return

        stream = req.get("stream", False)
        model = req.get("model", "mock-gpt")

        # 模拟延迟 50-150ms
        time.sleep(random.uniform(0.05, 0.15))

        if stream:
            self.send_sse_stream(model)
        else:
            self.send_sync_response(model)

    def send_sync_response(self, model):
        resp = {
            "id": f"chatcmpl-{random.randint(10**9, 10**10)}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": TOTAL_RESPONSE},
                    "finish_reason": "stop",
                }
            ],
            "usage": {
                "prompt_tokens": 23,
                "completion_tokens": len(TOTAL_RESPONSE.split()),
                "total_tokens": 23 + len(TOTAL_RESPONSE.split()),
            },
        }
        data = json.dumps(resp)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data.encode())

    def send_sse_stream(self, model):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.send_header("X-Accel-Buffering", "no")
        self.end_headers()

        # 初始 chunk
        self._sse({"id": f"chatcmpl-{random.randint(10**9, 10**10)}",
                   "object": "chat.completion.chunk",
                   "created": int(time.time()), "model": model,
                   "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}}]})

        # 逐词发送（比逐字符快很多，不 sleep）
        words = TOTAL_RESPONSE.split(" ")
        for word in words:
            self._sse({"choices": [{"index": 0,
                                     "delta": {"content": word + " "}}]})

        # finish_reason
        self._sse({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def _sse(self, obj):
        self.wfile.write(f"data: {json.dumps(obj)}\n\n".encode())
        self.wfile.flush()


if __name__ == "__main__":
    server = HTTPServer((HOST, PORT), MockHandler)
    print(f"[mock-llm] listening on http://{HOST}:{PORT}")
    print(f"[mock-llm] 非流式: curl -X POST http://localhost:{PORT}/chat/completions ...")
    print(f"[mock-llm] 流式:   curl -X POST http://localhost:{PORT}/chat/completions -d '{{\"stream\":true,...}}'")
    server.serve_forever()
