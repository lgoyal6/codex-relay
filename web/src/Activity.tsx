// Activity: what was decided, which identity served it, why, and how it went. No prompt
// text, conversation bodies or tokens are stored, so none can be shown here.

import { useMemo, useState } from "react";
import type { ActivityRow, State } from "./api";
import { Badge, EmptyState, Expando, clockText, reasonText } from "./ui";
import { IconInbox, IconList } from "./icons";

// null means the value was never measured; 0 means it was measured as under a millisecond.
// `if (!v)` treated both as missing and hid a real measurement.
function ms(v: number | null | undefined): string {
  if (v === null || v === undefined) return "-";
  return v < 1000 ? `${v} ms` : `${(v / 1000).toFixed(2)} s`;
}

export function Activity({
  state,
  activity,
  loading,
}: {
  state: State;
  activity: ActivityRow[];
  loading: boolean;
}) {
  const [only, setOnly] = useState<"all" | "blocked" | "errors">("all");
  const nameOf = (id?: string) => (id ? state.workspaces.find((w) => w.id === id)?.name ?? id : "-");

  const rows = useMemo(() => {
    if (only === "blocked") return activity.filter((r) => r.outcome !== "selected");
    if (only === "errors") return activity.filter((r) => r.error_class || (r.status_code ?? 0) >= 400);
    return activity;
  }, [activity, only]);

  const summary = useMemo(() => {
    const served = activity.filter((r) => r.outcome === "selected" && !r.error_class);
    // A 0 ms first token is a real measurement, so filter on null rather than on falsiness.
    const firstTokens = served
      .map((r) => r.first_token_ms)
      .filter((v): v is number => v !== null && v !== undefined);
    const byModel = new Map<string, number>();
    for (const r of served) if (r.model) byModel.set(r.model, (byModel.get(r.model) ?? 0) + 1);
    const median =
      firstTokens.length === 0
        ? 0
        : [...firstTokens].sort((a, b) => a - b)[Math.floor(firstTokens.length / 2)];
    return { total: activity.length, served: served.length, median, byModel: [...byModel.entries()] };
  }, [activity]);

  return (
    <>
      <section className="card">
        <div className="card-head">
          <div>
            <p className="card-title" style={{ fontSize: 13 }}>
              <IconList className="ico-sm" />
              Activity
            </p>
            <p className="subline">
              The last {activity.length} decisions. History is bounded on purpose and older rows are
              discarded; this is a local log, not an archive.
            </p>
          </div>
          <div className="field" style={{ marginBottom: 0, minWidth: 150 }}>
            <label htmlFor="act-filter">Show</label>
            <select id="act-filter" value={only} onChange={(e) => setOnly(e.target.value as typeof only)}>
              <option value="all">All decisions</option>
              <option value="blocked">Blocked only</option>
              <option value="errors">Errors only</option>
            </select>
          </div>
        </div>
        <div className="row" style={{ gap: 20, marginTop: 12 }}>
          <div>
            <div className="quota-label">Decisions recorded</div>
            <div className="quota-pct">{summary.total}</div>
          </div>
          <div>
            <div className="quota-label">Served</div>
            <div className="quota-pct">{summary.served}</div>
          </div>
          <div>
            <div className="quota-label">Median first token</div>
            <div className="quota-pct">{summary.median ? ms(summary.median) : "-"}</div>
          </div>
          <div>
            <div className="quota-label">Models seen</div>
            <div className="quota-pct">
              {summary.byModel.length === 0
                ? "-"
                : summary.byModel.map(([m, n]) => `${m} (${n})`).join(", ")}
            </div>
          </div>
        </div>
        <p className="note" style={{ marginTop: 10 }}>
          Timings are measured at this proxy, so they include our own overhead and the upstream
          response. They are not a cost estimate: this tool does not know what your subscription
          spends.
        </p>
      </section>

      <section className="card">
        {loading && activity.length === 0 ? (
          <div className="empty" aria-busy="true">
            <div className="skeleton" style={{ width: 220, height: 13 }} />
            <div className="skeleton" style={{ width: 300, height: 13 }} />
          </div>
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<IconInbox className="ico-lg" />}
            title={activity.length === 0 ? "No decisions yet" : "Nothing matches this filter"}
          >
            {activity.length === 0
              ? "Decisions appear as Codex sends requests through codex-relay."
              : "Try a different filter to see more rows."}
          </EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Decision</th>
                  <th>Workspace</th>
                  <th>Model</th>
                  <th className="num">Attempt</th>
                  <th className="num">First token</th>
                  <th className="num">Total</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className="mono">{clockText(r.at)}</td>
                    <td style={{ maxWidth: 380 }}>
                      <div className="row" style={{ gap: 6, marginBottom: 2 }}>
                        {/* A workspace was chosen and the request still failed: that is not
                            "served". The badge follows what happened, not what was decided. */}
                        {r.outcome !== "selected" ? (
                          <Badge kind="danger">blocked</Badge>
                        ) : r.error_class ? (
                          <Badge kind="warn">not delivered</Badge>
                        ) : (
                          <Badge kind="ok">served</Badge>
                        )}
                        <span className="note">{reasonText(r.reason)}</span>
                        {r.error_class && <Badge kind="warn">{r.error_class}</Badge>}
                        {r.status_code ? <span className="note mono">HTTP {r.status_code}</span> : null}
                      </div>
                      <div>{r.summary}</div>
                      <Expando summary="Explanation">
                        {r.thread_id && <div className="note mono">conversation {r.thread_id}</div>}
                        {r.candidates.map((c) => (
                          <div key={c.workspace_id} className="row" style={{ gap: 8, padding: "2px 0" }}>
                            <strong style={{ minWidth: 140 }}>{nameOf(c.workspace_id)}</strong>
                            {c.eligible ? (
                              <Badge kind="ok">eligible{c.rank ? ` (rank ${c.rank})` : ""}</Badge>
                            ) : (
                              <Badge kind="warn">{reasonText(c.reason)}</Badge>
                            )}
                            {c.detail && <span className="note">{c.detail}</span>}
                          </div>
                        ))}
                        {r.notes.map((n, i) => (
                          <div key={i} style={{ marginTop: 6 }}>
                            <div className="note">{n.message}</div>
                            {n.comparison && <div className="compare">{n.comparison}</div>}
                          </div>
                        ))}
                      </Expando>
                    </td>
                    <td>{nameOf(r.workspace_id)}</td>
                    <td className="mono">{r.model ?? "-"}</td>
                    <td className="num">{r.attempt}</td>
                    <td className="num">{ms(r.first_token_ms)}</td>
                    <td className="num">{ms(r.total_ms)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}
