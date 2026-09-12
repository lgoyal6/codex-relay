# codex-pool vs codex-lb: proxy overhead

Measured 2026-09-12. Both proxies, same mock upstream (127.0.0.1:8801), same harness
(`bench.py`), same request body, arms interleaved so host drift hits both equally.

## Setup

- Mock answers `GET /backend-api/codex/responses` with a real WebSocket 101 (no rate-limit
  headers on the upgrade, quota in-band as `codex.rate_limits`) and `POST .../responses`
  with SSE. Both transports pace events identically: 50 ms between each of 3 events.
- codex-pool: isolated instance on :7810, own CODEXPOOL_HOME, synthetic credentials.
- codex-lb: isolated instance on :2456, own data dir. The user's live codex-lb on :2455
  was not touched; its store.db was read from a copy.
- Client POSTs and reads SSE. codex-pool relays HTTP->HTTP; codex-lb bridges HTTP->WS.

## Added time-to-first-byte (p50, vs direct-to-mock baseline)

| concurrency | codex-pool | codex-lb |
|---|---|---|
| 1 | +2.6 ms | +21.8 ms |
| 4 | +6.3 ms | +52.7 ms |
| 8 | +4.8 ms | +117.2 ms |

p95: codex-pool +3.7 / +10.2 / +8.1 ms; codex-lb +26.9 / +65.7 / +184.8 ms.

codex-pool stays flat as concurrency rises. codex-lb grows roughly linearly.

## Checks that were run before believing the number

- **Is it conversation-setup cost?** No. Reusing one thread-id across 15 turns gave
  codex-lb 36.1 ms p50 vs 36.5 ms for a new conversation each turn. The cost is per turn.
- **Is codex-pool skipping work?** No. Both persist every turn: 283 decisions in
  codex-pool's bench DB, 157 request_logs rows in codex-lb's.
- **A `/codex/models` comparison was discarded as unfair.** codex-lb serves 16 017 bytes
  from its own model registry with zero upstream calls; codex-pool relays 849 bytes. That
  measures different work, so it is not reported as a result.

## What this does NOT show

- The `total` column is unusable: codex-lb closes the stream immediately after the final
  event while the mock's SSE path sleeps 50 ms after it, so codex-lb shows *negative*
  added total at low concurrency. Artifact, not a speedup.
- Against real upstream, first token is ~1.9 s (codex-lb's own telemetry, 2399 real
  websocket turns: mean first-upstream-event 1887 ms, median first token 3958 ms). A
  20-40 ms proxy difference is under 1-2% of a real turn and will not be felt by one user.
- codex-lb's own recorded internal queueing on real traffic is 0 ms (bridge_queue_wait and
  response_create_gate_wait both average 0). Its real-world latency is upstream/model time,
  not proxy time.
- codex-lb records substantially richer per-request telemetry, which is part of what its
  overhead buys.

## Honest conclusion

codex-pool's proxy overhead is roughly 5-20x lower and, unlike codex-lb's, does not grow
with concurrency. For a single user on real upstream this is not perceptible. It only
starts to matter for concurrent or shared use. Latency is not the reason to prefer
codex-pool over codex-lb.
