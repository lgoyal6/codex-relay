"""Minimal RFC6455 client: enough to drive the codex responses WS path."""
import base64, hashlib, json, os, socket, struct


def _encode(payload: bytes, opcode: int = 0x1) -> bytes:
    n = len(payload)
    mask = os.urandom(4)
    head = bytes([0x80 | opcode])
    if n < 126:
        head += bytes([0x80 | n])
    elif n < (1 << 16):
        head += bytes([0x80 | 126]) + n.to_bytes(2, "big")
    else:
        head += bytes([0x80 | 127]) + n.to_bytes(8, "big")
    return head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload))


class WS:
    def __init__(self, host, port, path, headers=None, timeout=60):
        self.key = base64.b64encode(os.urandom(16)).decode()
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        req = [f"GET {path} HTTP/1.1", f"Host: {host}:{port}",
               "Upgrade: websocket", "Connection: Upgrade",
               f"Sec-WebSocket-Key: {self.key}", "Sec-WebSocket-Version: 13",
               "openai-beta: responses_websockets=2026-02-06"]
        for k, v in (headers or {}).items():
            req.append(f"{k}: {v}")
        self.sock.sendall(("\r\n".join(req) + "\r\n\r\n").encode())
        self.buf = b""
        while b"\r\n\r\n" not in self.buf:
            d = self.sock.recv(4096)
            if not d:
                raise ConnectionError("closed during handshake")
            self.buf += d
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        self.status = int(head.split(b" ")[1])
        if self.status != 101:
            raise ConnectionError(f"upgrade refused: {self.status}")

    def _recv_exact(self, n):
        while len(self.buf) < n:
            d = self.sock.recv(65536)
            if not d:
                raise ConnectionError("closed")
            self.buf += d
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send_json(self, obj):
        self.sock.sendall(_encode(json.dumps(obj).encode()))

    def recv(self):
        h = self._recv_exact(2)
        opcode = h[0] & 0x0F
        n = h[1] & 0x7F
        if n == 126:
            n = int.from_bytes(self._recv_exact(2), "big")
        elif n == 127:
            n = int.from_bytes(self._recv_exact(8), "big")
        return opcode, self._recv_exact(n) if n else b""

    def close(self):
        try:
            self.sock.close()
        except Exception:
            pass
