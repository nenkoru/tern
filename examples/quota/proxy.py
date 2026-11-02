#!/usr/bin/env python3
"""A stand-in for a metered proxy in front of a shared API key (demo-quota.sh).

The key holder runs it on their own machine: the key never leaves it.
It answers POST /v1/chat/completions like an OpenAI-compatible API (a canned
reply, with a usage block) and counts tokens per member (X-Member header);
GET /usage returns the counts. A real one forwards to the provider and
admits only members (a short-lived token checked against the group's set:
docs/COLLABORATIONS.md, "group pass"). Run: proxy.py PORT
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

USAGE = {}


class Proxy(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/usage":
            return self.reply(200, USAGE)
        self.reply(404, {"error": "not here"})

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            return self.reply(404, {"error": "not here"})
        member = self.headers.get("X-Member", "")
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        prompt = " ".join(m.get("content", "") for m in req.get("messages", []))
        p, c = 3 * len(prompt.split()) + 5, 120
        USAGE[member] = USAGE.get(member, 0) + p + c
        self.reply(200, {"choices": [{"message": {"role": "assistant", "content": "(a canned answer)"}}],
                         "usage": {"prompt_tokens": p, "completion_tokens": c, "total_tokens": p + c}})


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", int(sys.argv[1])), Proxy).serve_forever()
