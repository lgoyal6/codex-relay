#!/usr/bin/env python3
"""Mock ChatGPT backend-api upstream for the codex-pool end-to-end run.

Differences from the Stage A mock (materials/compat/mock_upstream.py, kept as evidence):
  * /codex/models includes supported_reasoning_levels, which codex-cli 0.154.0 needs to
    decode the response.
  * Rate-limit headers are PER ACCOUNT, selected by the chatgpt-account-id header, so a
    reserve rule can actually be driven.
  * A control endpoint lets the harness change an account's quota without a restart.
  * A turn whose input contains SLOWTURN streams slowly, so client cancellation can be
    observed as a real upstream disconnect.

No real OpenAI traffic and no quota consumed.
"""
import json
import os
import sys
import threading
import time
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.environ.get("MOCK_LOG", "requests.jsonl")
PORT = int(os.environ.get("MOCK_PORT", "8791"))
_lock = threading.Lock()

# used_percent per account, mutable through /_control/quota.
QUOTA = {
    "acct_personal": {"primary": 24.5, "secondary": 72.0},
    "acct_work": {"primary": 12.0, "secondary": 38.0},
}
# Absolute unix seconds, matching the live protocol (not a duration).
RESET = {
    "acct_personal": {"primary": 3600, "secondary": 5 * 86400},
    "acct_work": {"primary": 3600, "secondary": 2 * 86400},
}
DISCONNECTS = []
# MODE drives injected behaviour for the next generation requests. Default "ok".
# Set through POST /_control/mode {"mode": "...", "n": <count>}.
MODE = {"mode": "ok", "n": 0}


def record(entry):
    with _lock:
        with open(LOG, "a") as fh:
            fh.write(json.dumps(entry) + "\n")
            fh.flush()


def rate_limit_headers(account):
    q = QUOTA.get(account, QUOTA["acct_personal"])
    r = RESET.get(account, RESET["acct_personal"])
    now = int(time.time())
    return {
        "x-codex-primary-used-percent": str(q["primary"]),
        "x-codex-primary-window-minutes": "300",
        "x-codex-primary-reset-at": str(now + r["primary"]),
        "x-codex-secondary-used-percent": str(q["secondary"]),
        "x-codex-secondary-window-minutes": "10080",
        "x-codex-secondary-reset-at": str(now + r["secondary"]),
        "x-codex-credits-has-credits": "false",
        "x-codex-credits-unlimited": "false",
    }


def ws_rate_limit_payload(account):
    """The in-band form of the same quota the headers carry."""
    q = QUOTA.get(account, QUOTA["acct_personal"])
    r = RESET.get(account, RESET["acct_personal"])
    return {
        "primary": {"used_percent": q["primary"], "window_minutes": 300,
                    "resets_at": int(time.time()) + r["primary"]},
        "secondary": {"used_percent": q["secondary"], "window_minutes": 10080,
                      "resets_at": int(time.time()) + r["secondary"]},
    }


MODELS = {
    # The full ModelInfo field set codex-cli 0.154.0 requires. A partial entry makes the
    # client reject the whole catalog ("missing field `shell_type`"), which is how a mock
    # can silently stop being representative.
    "models": [
        {
            "slug": "gpt-5.1-codex",
            "display_name": "GPT-5.1-Codex",
            "description": None,
            "supported_reasoning_levels": [
                {"effort": "low", "description": "Fastest"},
                {"effort": "medium", "description": "Balanced"},
                {"effort": "high", "description": "Most thorough"},
            ],
            "default_reasoning_level": "medium",
            "shell_type": "unified_exec",
            "visibility": "list",
            "supported_in_api": True,
            "priority": 1,
            "upgrade": None,
            "model_messages": None,
            "default_reasoning_summary": "auto",
            "support_verbosity": False,
            "default_verbosity": None,
            "apply_patch_tool_type": None,
            "truncation_policy": {"mode": "bytes", "limit": 10000},
            "supports_image_detail_original": False,
            "context_window": None,
            "auto_compact_token_limit": None,
            "effective_context_window_percent": 95,
            "experimental_supported_tools": [],
            # Required: the client rejects a model carrying neither base_instructions nor
            # model_messages.instructions_template.
            "base_instructions": "You are a helpful assistant.",
        }
    ]
}


def tool_call_events():
    """A single function_call turn. codex-cli 0.154.0 names the shell tool exec_command and
    takes {"cmd": "..."}; arguments are a JSON *string*, not an object (protocol/src/models.rs)."""
    return [
        ("response.created", {"type": "response.created", "response": {"id": "resp_tool_1"}}),
        (
            "response.output_item.done",
            {
                "type": "response.output_item.done",
                "item": {
                    "type": "function_call",
                    "name": "exec_command",
                    "arguments": json.dumps({"cmd": "printf TOOLCALL_RAN > tool_evidence.txt && echo WROTE"}),
                    "call_id": "call_mock_1",
                },
            },
        ),
        (
            "response.completed",
            {
                "type": "response.completed",
                "response": {
                    "id": "resp_tool_1",
                    "usage": {
                        "input_tokens": 12,
                        "input_tokens_details": {"cached_tokens": 0},
                        "output_tokens": 5,
                        "output_tokens_details": {"reasoning_tokens": 0},
                        "total_tokens": 17,
                    },
                },
            },
        ),
    ]


# --- minimal RFC6455 server side -------------------------------------------------
# The real ChatGPT upstream answers GET /backend-api/codex/responses with a WebSocket
# upgrade, and carries NO rate-limit headers on the 101 -- quota arrives in-band as
# codex.rate_limits events. This mock reproduces both facts so either proxy can be
# measured against the same upstream behaviour.
WS_GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def ws_accept_key(key: str) -> str:
    return base64.b64encode(hashlib.sha1(key.encode() + WS_GUID).digest()).decode()


def ws_encode(payload: bytes, opcode: int = 0x1) -> bytes:
    n = len(payload)
    head = bytes([0x80 | opcode])
    if n < 126:
        head += bytes([n])
    elif n < (1 << 16):
        head += bytes([126]) + n.to_bytes(2, "big")
    else:
        head += bytes([127]) + n.to_bytes(8, "big")
    return head + payload


def ws_read_frame(rfile):
    """Return (opcode, payload) or None when the peer is gone."""
    hdr = rfile.read(2)
    if len(hdr) < 2:
        return None
    opcode = hdr[0] & 0x0F
    masked = hdr[1] & 0x80
    n = hdr[1] & 0x7F
    if n == 126:
        n = int.from_bytes(rfile.read(2), "big")
    elif n == 127:
        n = int.from_bytes(rfile.read(8), "big")
    mask = rfile.read(4) if masked else b""
    data = rfile.read(n) if n else b""
    if masked and data:
        data = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
    return opcode, data


def sse_events(text="MOCK_OK"):
    return [
        ("response.created", {"type": "response.created", "response": {"id": "resp_mock_1"}}),
        (
            "response.output_item.done",
            {
                "type": "response.output_item.done",
                "item": {
                    "type": "message",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": text}],
                },
            },
        ),
        (
            "response.completed",
            {
                "type": "response.completed",
                "response": {
                    "id": "resp_mock_1",
                    "usage": {
                        "input_tokens": 10,
                        "input_tokens_details": {"cached_tokens": 0},
                        "output_tokens": 3,
                        "output_tokens_details": {"reasoning_tokens": 0},
                        "total_tokens": 13,
                    },
                },
            },
        ),
    ]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    # Without this, this mock adds roughly 35 ms to every request made by a Go client while
    # adding nothing measurable for a Python one, because Nagle on the server socket
    # interacts with delayed ACK. That is a property of the harness, not of anything under
    # test, and it made codex-pool look ~100x slower than it is. Measured: 35 ms with Nagle
    # on versus 0.29 ms against an equivalent Go upstream.
    disable_nagle_algorithm = True

    def log_message(self, *_args):
        pass

    def account(self):
        return self.headers.get("chatgpt-account-id") or ""

    def _capture(self, body=b"", extra=None):
        entry = {
            "ts": time.time(),
            "method": self.command,
            "path": self.path,
            "account": self.account(),
            # The token itself is never logged; only enough to prove it was replaced.
            "auth_tail": (self.headers.get("authorization") or "")[-12:],
            "headers": {k.lower(): v for k, v in self.headers.items() if k.lower() != "authorization"},
            "body_len": len(body),
            "body_preview": body[:2000].decode("utf-8", "replace"),
        }
        entry.update(extra or {})
        record(entry)

    def _read_body(self):
        n = int(self.headers.get("content-length") or 0)
        return self.rfile.read(n) if n else b""

    def _json(self, obj, status=200, extra=None):
        payload = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self._capture()
        p = self.path.split("?")[0]
        if p == "/_control/state":
            self._json({"quota": QUOTA, "disconnects": DISCONNECTS})
        elif p.endswith("/codex/models"):
            self._json(MODELS, extra=rate_limit_headers(self.account()))
        elif p.endswith("/wham/accounts/check"):
            self._json(
                {
                    "accounts": [
                        {"id": "acct_personal", "name": "Personal", "structure": "personal"},
                        {"id": "acct_work", "name": "Acme Work", "structure": "workspace"},
                    ],
                    "account_ordering": ["acct_personal", "acct_work"],
                    "default_account_id": "acct_personal",
                }
            )
        elif p.endswith("/wham/usage"):
            acct = self.account() or "acct_personal"
            q = QUOTA.get(acct, QUOTA["acct_personal"])
            r = RESET.get(acct, RESET["acct_personal"])
            now = int(time.time())
            # Field names here are NOT the ones the headers or the in-band events use. This
            # endpoint reports primary_window/secondary_window and states the period as
            # limit_window_seconds. Confirmed against a live response; an earlier version of
            # this mock invented "rate_limits"/"window_minutes" and a parser written against
            # it agreed with the mock and failed against the real endpoint.
            self._json(
                {
                    "user_id": "user_mock",
                    "account_id": acct,
                    "email": "mock@example.com",
                    "plan_type": "plus",
                    "rate_limit": {
                        "allowed": True,
                        "limit_reached": False,
                        "primary_window": {
                            "used_percent": q["primary"],
                            "limit_window_seconds": 18000,
                            "reset_after_seconds": r["primary"],
                            "reset_at": now + r["primary"],
                        },
                        "secondary_window": {
                            "used_percent": q["secondary"],
                            "limit_window_seconds": 604800,
                            "reset_after_seconds": r["secondary"],
                            "reset_at": now + r["secondary"],
                        },
                    },
                    "credits": {"has_credits": False, "unlimited": False, "balance": "0"},
                    "rate_limit_reached_type": None,
                }
            )
        elif p.endswith("/codex/responses"):
            if (self.headers.get("upgrade") or "").lower() == "websocket":
                self._serve_ws()
            else:
                # codex-lb's "http" transport opens a plain GET bridge connection here.
                self.send_response(200)
                self.send_header("content-type", "text/event-stream")
                self.send_header("cache-control", "no-store")
                for k, v in rate_limit_headers(self.account()).items():
                    self.send_header(k, v)
                self.send_header("transfer-encoding", "chunked")
                self.end_headers()
                try:
                    deadline = time.time() + 120
                    while time.time() < deadline:
                        chunk = b": keepalive\n\n"
                        self.wfile.write(hex(len(chunk))[2:].encode() + b"\r\n" + chunk + b"\r\n")
                        self.wfile.flush()
                        time.sleep(1.0)
                    self.wfile.write(b"0\r\n\r\n")
                except Exception:
                    pass
        else:
            self._json({"mock": "unhandled", "path": p}, status=404)

    def _serve_ws(self):
        """Answer the upgrade, then emit response events for each response.create."""
        key = self.headers.get("sec-websocket-key")
        if not key:
            self._json({"mock": "missing sec-websocket-key"}, status=400)
            return
        self.close_connection = True
        self.send_response(101)
        self.send_header("upgrade", "websocket")
        self.send_header("connection", "Upgrade")
        self.send_header("sec-websocket-accept", ws_accept_key(key))
        # Deliberately NO rate-limit headers here: the real 101 carries none.
        self.end_headers()
        self.wfile.flush()

        acct = self.headers.get("chatgpt-account-id") or "acct_personal"
        try:
            while True:
                frame = ws_read_frame(self.rfile)
                if frame is None:
                    return
                opcode, data = frame
                if opcode == 0x8:            # close
                    return
                if opcode == 0x9:            # ping -> pong
                    self.wfile.write(ws_encode(data, 0xA)); self.wfile.flush()
                    continue
                if opcode not in (0x1, 0x2):
                    continue
                try:
                    msg = json.loads(data or b"{}")
                except Exception:
                    continue
                if msg.get("type") not in ("response.create", None):
                    continue
                record({"method": "WS", "path": self.path, "type": msg.get("type")})
                # Quota arrives in-band on this transport, exactly as upstream does it.
                self.wfile.write(ws_encode(json.dumps({
                    "type": "codex.rate_limits",
                    "rate_limits": ws_rate_limit_payload(acct),
                }).encode()))
                # Same inter-event pacing as the SSE path, so the two transports differ
                # only in framing. Without this the WS arm looks faster than upstream
                # actually is and any proxy comparison built on it is meaningless.
                for _name, ev in sse_events():
                    self.wfile.write(ws_encode(json.dumps(ev).encode()))
                    self.wfile.flush()
                    time.sleep(0.05)
        except Exception:
            return

    def do_POST(self):
        body = self._read_body()
        p = self.path.split("?")[0]

        if p == "/_control/mode":
            payload = json.loads(body or b"{}")
            MODE["mode"] = payload.get("mode", "ok")
            MODE["n"] = int(payload.get("n", 1))
            self._json({"mode": MODE})
            return

        if p == "/_control/quota":
            payload = json.loads(body or b"{}")
            for acct, vals in payload.items():
                QUOTA.setdefault(acct, {"primary": 0.0, "secondary": 0.0}).update(vals)
            self._json({"quota": QUOTA})
            return

        self._capture(body)
        if p.endswith("/responses"):
            # An injected mode applies to the next n generation requests, then expires.
            mode = "ok"
            if MODE["n"] > 0:
                mode = MODE["mode"]
                MODE["n"] -= 1

            if mode in ("fail401", "fail429", "fail500"):
                status = int(mode[4:])
                extra = dict(rate_limit_headers(self.account()))
                if status == 429:
                    extra["x-codex-rate-limit-reached-type"] = "rate_limit_reached"
                self._json({"error": {"message": f"injected {status}", "type": "mock"}},
                           status=status, extra=extra)
                return

            # Tool mode: the first request gets a function call, the follow-up carrying the
            # tool output gets a normal answer. Keyed off the body so it is stateless.
            events = sse_events()
            if mode == "tool" or b"function_call_output" in body:
                events = (
                    sse_events("TOOLCALL_DONE")
                    if b"function_call_output" in body
                    else tool_call_events()
                )

            slow = b"SLOWTURN" in body
            self.send_response(200)
            self.send_header("content-type", "text/event-stream")
            self.send_header("cache-control", "no-store")
            for k, v in rate_limit_headers(self.account()).items():
                self.send_header(k, v)
            self.send_header("transfer-encoding", "chunked")
            self.end_headers()
            try:
                for name, data in events:
                    chunk = f"event: {name}\ndata: {json.dumps(data)}\n\n".encode()
                    self.wfile.write(hex(len(chunk))[2:].encode() + b"\r\n" + chunk + b"\r\n")
                    self.wfile.flush()
                    time.sleep(4.0 if slow else 0.05)
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError) as exc:
                # This is the observable proof that a client cancellation propagated all the
                # way through the proxy to the upstream connection.
                DISCONNECTS.append({"ts": time.time(), "path": p, "slow": slow, "error": type(exc).__name__})
                record({"ts": time.time(), "event": "upstream_disconnected", "path": p, "slow": slow})
        else:
            self._json({"mock": "unhandled", "path": p}, status=404)


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"mock upstream on 127.0.0.1:{PORT}", file=sys.stderr, flush=True)
    srv.serve_forever()
