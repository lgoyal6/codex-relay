// Activity: what was decided, which identity served it, why, and how it went. No prompt
// text or conversation bodies are stored. Token counts are the upstream's reported usage.

import { useMemo, useState } from "react";
import type { ActivityRow, ModelReportRow, State } from "./api";
import { api } from "./api";
import { Badge, EmptyState, Expando, fullStamp, pct, reasonText, stampText } from "./ui";
import { IconInbox, IconList } from "./icons";

// null means the value was never measured; 0 means it was measured as under a millisecond.
// `if (!v)` treated both as missing and hid a real measurement.
function ms(v: number | null | undefined): string {
  if (v === null || v === undefined) return "-";
  return v < 1000 ? `${v} ms` : `${(v / 1000).toFixed(2)} s`;
}

function shortThread(id?: string): string {
  if (!id) return "-";
  if (id.length <= 14) return id;
  return `${id.slice(0, 5)}…${id.slice(-8)}`;
}

function tokens(r: ActivityRow): string {
  if (r.total_tokens === null || r.total_tokens === undefined) return "-";
  return `${r.total_tokens.toLocaleString()} total`;
}

function throughput(v: number | null | undefined): string {
  return v === null || v === undefined ? "-" : `${v.toFixed(1)} tok/s`;
}

export function Activity({
  state,
  activity,
  models,
  loading,
}: {
  state: State;
  activity: ActivityRow[];
  models: ModelReportRow[];
  loading: boolean;
}) {
  const [only, setOnly] = useState<"all" | "subagents" | "blocked" | "errors">("all");
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);
  const nameOf = (id?: string) => (id ? state.workspaces.find((w) => w.id === id)?.name ?? id : "-");
  const quotaAtTurn = (r: ActivityRow) => {
    if (!r.quota_snapshot) return "not recorded";
    if (r.quota_snapshot.windows.length === 0) return "no quota reported";
    // Used, not remaining. "83% used" is the number a person is watching climb; the
    // remaining figure is kept beside it because that is what the rules are written against.
    return r.quota_snapshot.windows
      .map((w) => `${w.label} ${pct(100 - w.remaining_percent)} used (${pct(w.remaining_percent)} left)`)
      .join(" · ");
  };

  const exportCSV = async () => {
    setExporting(true);
    setExportError(null);
    try {
      const blob = await api.activityCSV();
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = "codexrelay-activity.csv";
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
    } catch (error) {
      setExportError(error instanceof Error ? error.message : String(error));
    } finally {
      setExporting(false);
    }
  };

  const rows = useMemo(() => {
    if (only === "subagents") return activity.filter((r) => r.request_kind === "subagent");
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
    const median =
      firstTokens.length === 0
        ? 0
        : [...firstTokens].sort((a, b) => a - b)[Math.floor(firstTokens.length / 2)];
    return { total: activity.length, served: served.length, median };
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
          <div className="row" style={{ gap: 10 }}>
            <button className="btn" type="button" disabled={exporting} onClick={exportCSV}>
              {exporting ? "Exporting..." : "Export CSV"}
            </button>
            <div className="field" style={{ marginBottom: 0, minWidth: 150 }}>
              <label htmlFor="act-filter">Show</label>
              <select id="act-filter" value={only} onChange={(e) => setOnly(e.target.value as typeof only)}>
                <option value="all">All decisions</option>
                <option value="subagents">Subagents only</option>
                <option value="blocked">Blocked only</option>
                <option value="errors">Errors only</option>
              </select>
            </div>
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
            <div className="quota-pct">{models.length || "-"}</div>
          </div>
        </div>
        {exportError && <p className="note" style={{ marginTop: 10 }}>{exportError}</p>}
        <p className="note" style={{ marginTop: 10 }}>
          Timings are measured at this proxy, so they include our own overhead and the upstream
          response. They are not a cost estimate: this tool does not know what your subscription
          spends.
        </p>
      </section>

      <section className="card">
        <div className="card-head">
          <div>
            <p className="card-title" style={{ fontSize: 13 }}>Model performance</p>
            <p className="subline">
              Completed served turns across the retained decision history. Error, cancelled, and blocked rows are excluded.
            </p>
          </div>
        </div>
        {models.length === 0 ? (
          <div className="empty">No completed model turns with retained history yet.</div>
        ) : (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>Model</th>
                  <th className="num">Completed turns</th>
                  <th className="num">Usage coverage</th>
                  <th className="num">Tokens</th>
                  <th className="num">Median first token</th>
                  <th className="num">Measured output speed</th>
                </tr>
              </thead>
              <tbody>
                {models.map((model) => (
                  <tr key={model.model}>
                    <td className="mono">{model.model}</td>
                    <td className="num">{model.turns.toLocaleString()}</td>
                    <td className="num">{model.turns_with_usage.toLocaleString()} / {model.turns.toLocaleString()}</td>
                    <td className="num">{model.total_tokens.toLocaleString()}</td>
                    <td className="num">{ms(model.median_first_token_ms)}</td>
                    <td className="num">{throughput(model.output_tokens_per_second)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="note" style={{ marginTop: 10 }}>
          The report covers at most the 2,000 retained decisions. Output speed is measured only when output tokens and both timing points were reported.
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
                  <th>Chat</th>
                  <th>Decision</th>
                  <th>Workspace</th>
                  <th className="num">Tokens</th>
                  <th>Quota at turn</th>
                  <th>Model</th>
                  <th className="num">Attempt</th>
                  <th className="num">First token</th>
                  <th className="num">Total</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className="mono" title={fullStamp(r.at)}>{stampText(r.at)}</td>
                    <td className="mono" title={r.thread_id ?? "No thread id reported"}>
                      <div>{shortThread(r.thread_id)}</div>
                      <Badge kind={r.request_kind === "subagent" ? "info" : "neutral"}>
                        {r.request_kind === "subagent" ? "subagent" : "parent"}
                      </Badge>
                    </td>
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
                        {r.total_tokens !== null && r.total_tokens !== undefined && (
                          <div className="note mono">
                            input {r.input_tokens?.toLocaleString() ?? 0} · cached{" "}
                            {r.cached_input_tokens?.toLocaleString() ?? 0} · output{" "}
                            {r.output_tokens?.toLocaleString() ?? 0} · total {r.total_tokens.toLocaleString()}
                          </div>
                        )}
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
                    <td>
                      <div>{r.workspace_name || nameOf(r.workspace_id)}</div>
                      {r.account_email && (
                        <div className="note mono">{r.account_email}{r.account_plan ? ` · ${r.account_plan}` : ""}</div>
                      )}
                    </td>
                    <td className="num" title="Input, cached input, output, and total are in the explanation">
                      {tokens(r)}
                    </td>
                    <td title={r.quota_snapshot ? `Captured ${fullStamp(r.quota_snapshot.captured_at)}` : "Historical snapshot unavailable"}>
                      {quotaAtTurn(r)}
                    </td>
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
