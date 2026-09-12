#!/usr/bin/env python3
"""Measure codex-pool's added latency against a controlled upstream.

Both arms hit the SAME mock upstream with the SAME payload over the SAME transport. The only
difference is whether the request goes direct or through codex-pool. What is reported is the
DIFFERENCE, which is the proxy's own cost; the upstream's own time cancels out.

Measured separately:
  ttfb  - time to first response byte. For a streamed turn this is what a person feels.
  total - time until the stream ends.
"""
import argparse
import http.client
import json
import statistics
import sys
import threading
import time
from urllib.parse import urlparse

BODY = json.dumps({
    "model": "gpt-5.1-codex",
    "stream": True,
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "hello " * 200}]}],
}).encode()


def one(url: str, thread_id: str, method: str = "POST") -> tuple[float, float, int]:
    u = urlparse(url)
    conn = http.client.HTTPConnection(u.hostname, u.port, timeout=60)
    headers = {
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
        "thread-id": thread_id,
        "session-id": thread_id,
        "Authorization": "Bearer CLIENT_TOKEN",
        "ChatGPT-Account-ID": "acct_personal",
    }
    t0 = time.perf_counter()
    if method == "GET":
        headers.pop("Content-Type", None)
        conn.request("GET", u.path, headers=headers)
    else:
        conn.request("POST", u.path, body=BODY, headers=headers)
    resp = conn.getresponse()
    first = None
    # read1() returns as soon as ONE chunk is available. Plain read(n) would block until n
    # bytes accumulated, which on a slow SSE stream waits for the whole response and would
    # make time-to-first-byte indistinguishable from total. That mistake was made once here
    # and is what this comment exists to prevent.
    while True:
        chunk = resp.read1(65536)
        if first is None and chunk:
            first = time.perf_counter()
        if not chunk:
            break
    t1 = time.perf_counter()
    status = resp.status
    conn.close()
    if first is None:
        first = t1
    return (first - t0) * 1000, (t1 - t0) * 1000, status


def run(url: str, n: int, concurrency: int, tag: str, method: str = "POST") -> dict:
    ttfbs: list[float] = []
    totals: list[float] = []
    statuses: list[int] = []
    lock = threading.Lock()
    counter = {"i": 0}

    def worker(w: int) -> None:
        while True:
            with lock:
                i = counter["i"]
                if i >= n:
                    return
                counter["i"] = i + 1
            # A distinct thread id per request: every request is a NEW conversation, so the
            # ownership write is included in the measurement rather than avoided.
            f, t, s = one(url, f"bench-{tag}-{i}", method)
            with lock:
                ttfbs.append(f)
                totals.append(t)
                statuses.append(s)

    threads = [threading.Thread(target=worker, args=(w,)) for w in range(concurrency)]
    t0 = time.perf_counter()
    for th in threads:
        th.start()
    for th in threads:
        th.join()
    wall = time.perf_counter() - t0
    ok = sum(1 for s in statuses if s == 200)
    return {
        "tag": tag, "n": n, "concurrency": concurrency, "ok": ok,
        "ttfb_p50": statistics.median(ttfbs), "ttfb_p95": pct(ttfbs, 95),
        "total_p50": statistics.median(totals), "total_p95": pct(totals, 95),
        "wall_s": wall, "rps": n / wall if wall else 0,
    }


def pct(xs: list[float], p: int) -> float:
    s = sorted(xs)
    k = max(0, min(len(s) - 1, int(round((p / 100) * (len(s) - 1)))))
    return s[k]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--method", default="POST", choices=["POST", "GET"])
    ap.add_argument("--direct", required=True)
    ap.add_argument("--proxied", required=True)
    ap.add_argument("-n", type=int, default=40)
    ap.add_argument("--warmup", type=int, default=5)
    ap.add_argument("--concurrency", type=int, nargs="+", default=[1, 4])
    args = ap.parse_args()

    for _ in range(args.warmup):
        one(args.direct, "warm-d", args.method)
        one(args.proxied, "warm-p", args.method)

    rows = []
    for c in args.concurrency:
        # Interleave the arms so drift in the host affects both equally.
        d = run(args.direct, args.n, c, f"direct-c{c}", args.method)
        p = run(args.proxied, args.n, c, f"proxied-c{c}", args.method)
        d2 = run(args.direct, args.n, c, f"direct2-c{c}", args.method)
        d["ttfb_p50"] = (d["ttfb_p50"] + d2["ttfb_p50"]) / 2
        d["total_p50"] = (d["total_p50"] + d2["total_p50"]) / 2
        d["ttfb_p95"] = (d["ttfb_p95"] + d2["ttfb_p95"]) / 2
        d["total_p95"] = (d["total_p95"] + d2["total_p95"]) / 2
        rows.append((c, d, p))

    print()
    print(f"{'conc':>4}  {'arm':<9} {'ok':>4}  {'ttfb p50':>9} {'ttfb p95':>9}  {'total p50':>10} {'total p95':>10}")
    print("-" * 68)
    out = []
    for c, d, p in rows:
        for r in (d, p):
            arm = "direct" if r["tag"].startswith("direct") else "codexpool"
            print(f"{c:>4}  {arm:<9} {r['ok']:>4}  {r['ttfb_p50']:>8.2f}ms {r['ttfb_p95']:>8.2f}ms"
                  f"  {r['total_p50']:>9.2f}ms {r['total_p95']:>9.2f}ms")
        add_ttfb = p["ttfb_p50"] - d["ttfb_p50"]
        add_total = p["total_p50"] - d["total_p50"]
        print(f"{'':>4}  {'ADDED':<9} {'':>4}  {add_ttfb:>+8.2f}ms {p['ttfb_p95'] - d['ttfb_p95']:>+8.2f}ms"
              f"  {add_total:>+9.2f}ms {p['total_p95'] - d['total_p95']:>+9.2f}ms")
        print("-" * 68)
        out.append({"concurrency": c, "direct": d, "proxied": p,
                    "added_ttfb_p50_ms": add_ttfb, "added_total_p50_ms": add_total})
    json.dump(out, open("bench-result.json", "w"), indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
