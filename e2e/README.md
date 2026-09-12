# End-to-end harness

A mock ChatGPT upstream and a latency harness. Neither needs a real account, and neither
reads your credentials.

## Mock upstream

```
MOCK_PORT=8801 python3 mock_upstream.py
```

Serves the routes codex-relay talks to: the model catalog, `wham/accounts/check`,
`wham/usage`, and generation over both transports. `POST /backend-api/codex/responses`
returns SSE; `GET` on the same path with an `Upgrade: websocket` header returns a real
RFC6455 101 and streams the same events as frames.

Two details are deliberate, because getting either wrong makes the mock unrepresentative:

- The 101 carries **no** rate-limit headers. Real upstream sends quota in-band as
  `codex.rate_limits` events on the websocket transport, and this mock does the same.
- Both transports pace events identically (50 ms apart). Without matching pacing, a proxy
  that uses one transport looks faster than upstream actually is.

## Latency harness

```
python3 bench/bench.py \
  --direct  http://127.0.0.1:8801/backend-api/codex/responses \
  --proxied http://127.0.0.1:7788/backend-api/codex/responses \
  -n 40 --concurrency 1 4 8
```

Measures added time-to-first-byte against a direct-to-upstream baseline, interleaving the
arms so host drift affects both equally. `--method GET` measures a non-streaming route.
See `docs/comparison.md` for a worked comparison and, more importantly, for the checks
that have to pass before a number from this harness means anything.
