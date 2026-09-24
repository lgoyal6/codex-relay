// Overview answers, in order: which workspace a new conversation would use and why; what
// every workspace has left; what needs attention; and what was decided recently.

import { useEffect, useState } from "react";
import type { ActivityRow, Decision, Rule, Simulation, State } from "./api";
import { ApiError, api } from "./api";
import {
  Badge,
  Card,
  EmptyState,
  ErrorBox,
  Expando,
  clockText,
  evidenceBadge,
  pct,
  reasonText,
  resetAtText,
} from "./ui";
import { Meter, Sparkline, SERIES_COLORS } from "./viz";
import {
  IconAlert,
  IconGauge,
  IconInbox,
  IconLayers,
  IconList,
  IconPencil,
  IconPin,
  IconShieldCheck,
  IconClock,
  IconInfo,
} from "./icons";

export function Overview({
  state,
  activity,
  now,
  onNavigate,
  onEditRule,
  onReconnect,
  reload,
}: {
  state: State;
  activity: ActivityRow[];
  now: Date;
  onNavigate: (tab: "workspaces" | "rules" | "activity" | "settings") => void;
  onEditRule: (ruleId: string) => void;
  onReconnect: () => void;
  reload: () => void;
}) {
  const [model, setModel] = useState("");
  const [decision, setDecision] = useState<Decision>(state.proposed);
  const [modelError, setModelError] = useState<string | null>(null);
  const [switchError, setSwitchError] = useState<string | null>(null);
  const [switchingTo, setSwitchingTo] = useState<string | null>(null);

  // With no model chosen the state response already carries the answer. With one chosen we
  // ask the service to evaluate it: same evaluator, same snapshot, no quota consumed.
  useEffect(() => {
    if (!model.trim()) {
      setDecision(state.proposed);
      setModelError(null);
      return;
    }
    let cancelled = false;
    const t = setTimeout(() => {
      api
        .preview(null, model.trim(), [])
        .then((r) => {
          if (!cancelled) {
            setDecision(r.decision);
            setModelError(null);
          }
        })
        .catch((e: unknown) => {
          if (!cancelled) setModelError(e instanceof ApiError ? e.message : String(e));
        });
    }, 250);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [model, state.version, state.proposed]);

  const nameOf = (id?: string) => state.workspaces.find((w) => w.id === id)?.name ?? id ?? "";
  const blocked = decision.outcome !== "selected";
  const reserveRule = state.rules.find((r) => r.kind === "reserve" && r.enabled);
  const preferenceRule = state.rules.find((r) => r.kind === "prefer");
  const activePreference = preferenceRule?.enabled ? preferenceRule : undefined;

  const preferWorkspace = async (workspaceID: string) => {
    const rule: Rule = preferenceRule
      ? { ...preferenceRule, enabled: true, source_workspace_id: workspaceID }
      : {
          id: "",
          kind: "prefer",
          enabled: true,
          priority: 0,
          source_workspace_id: workspaceID,
          window: { minutes: 0 },
          comparison: "at_or_below",
          remaining_percent: 0,
          reset_comparison: "at_least",
          reset_hours: 0,
          preferred_workspace_id: "",
          no_alternative: "stop_and_explain",
        };
    setSwitchingTo(workspaceID);
    setSwitchError(null);
    try {
      await api.saveRule(rule);
      reload();
    } catch (e) {
      setSwitchError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setSwitchingTo(null);
    }
  };

  // The headline is the service's own summary. The line under it says what is shaping that
  // answer, rather than repeating the sentence above it.
  const excluded = decision.candidates.filter(
    (c) => !c.eligible && (c.reason === "protected_by_reserve" || c.reason === "protection_evidence_unknown"),
  );
  const why =
    decision.primary_reason === "preferred_by_rule"
      ? "A rule prefers it."
      : decision.primary_reason === "owner_bound_conversation"
        ? "It already owns this conversation."
        : "It is your default workspace and no rule changes that.";
  const subline = blocked
    ? decision.summary
    : [...excluded.map((c) => `${nameOf(c.workspace_id)} is protected by your reserve rule.`), why].join(" ");

  return (
    <>
      <section className="card">
        <div className="hero">
          <div className="spread">
            <div className="grow">
              <div className="hero-eyebrow">
                <IconGauge className="ico-sm" />
                Next conversation
              </div>
              <h2>
                {blocked
                  ? "No workspace can take a new conversation"
                  : `New conversations will use ${nameOf(decision.workspace_id)}`}
              </h2>
              <p>{subline}</p>
              {decision.unknown_evidence && (
                <p style={{ marginTop: 8 }} className="row">
                  <Badge kind="warn">evidence unknown</Badge>
                  <span className="note">
                    A protection-sensitive quota reading is missing, so the reserve is held rather
                    than assumed clear.
                  </span>
                </p>
              )}
            </div>
            <div className="field" style={{ minWidth: 200, maxWidth: 240 }}>
              <label htmlFor="ov-model">Evaluate for model</label>
              <input
                id="ov-model"
                type="text"
                value={model}
                placeholder="any model"
                onChange={(e) => setModel(e.target.value)}
              />
              <span className="hint">
                Model eligibility is only known for workspaces that have reported one.
              </span>
            </div>
          </div>

          <ErrorBox error={modelError ?? switchError} />

          <div className="actions mt">
            {state.workspaces.map((workspace) => {
              const preferred = activePreference?.source_workspace_id === workspace.id;
              return (
                <button
                  key={workspace.id}
                  className={`btn${preferred ? " primary" : ""}`}
                  disabled={switchingTo !== null || preferred}
                  onClick={() => void preferWorkspace(workspace.id)}
                >
                  {switchingTo === workspace.id
                    ? "Switching…"
                    : preferred
                      ? `${workspace.name} preferred`
                      : `Prefer ${workspace.name}`}
                </button>
              );
            })}
            {reserveRule ? (
              <button className="btn" onClick={() => onEditRule(reserveRule.id)}>
                <IconPencil className="ico-sm" />
                Edit rule
              </button>
            ) : (
              <button className="btn primary" onClick={() => onNavigate("rules")}>
                <IconShieldCheck className="ico-sm" />
                Create a rule
              </button>
            )}
            <button className="btn" onClick={() => onNavigate("rules")}>
              <IconGauge className="ico-sm" />
              Preview routing
            </button>
          </div>
          <p className="note" style={{ marginTop: 8 }}>
            Changing the preferred account applies to new tasks immediately. Existing tasks move
            on their next turn when that account is eligible.
          </p>

          <div style={{ marginTop: 12 }}>
            <Expando summary="Why this workspace?">
              <ol style={{ margin: "0 0 10px", paddingLeft: 18 }}>
                {decision.candidates.map((c) => (
                  <li key={c.workspace_id} style={{ marginBottom: 6 }}>
                    <span className="row" style={{ gap: 6 }}>
                      <strong>{nameOf(c.workspace_id)}</strong>
                      {c.eligible ? (
                        <Badge kind="ok">eligible{c.rank ? ` (rank ${c.rank})` : ""}</Badge>
                      ) : (
                        <Badge kind="warn">{reasonText(c.reason)}</Badge>
                      )}
                    </span>
                    {c.detail && <div className="note">{c.detail}</div>}
                  </li>
                ))}
              </ol>
              {decision.notes.map((n, i) => (
                <div key={i} style={{ marginBottom: 8 }}>
                  <div className="note">{n.message}</div>
                  {n.comparison && <div className="compare">{n.comparison}</div>}
                </div>
              ))}
              <p className="note" style={{ margin: 0 }}>
                Evaluated at state version {decision.state_version}. A threshold is a selection
                trigger, not a spending cap: requests already running and usage elsewhere can still
                cross it.
              </p>
            </Expando>
          </div>
        </div>
      </section>

      <Card
        title="Accounts"
        icon={<IconLayers className="ico-sm" />}
        actions={
          <button className="btn sm ghost" onClick={() => onNavigate("workspaces")}>
            Manage
          </button>
        }
      >
        {state.workspaces.length === 0 ? (
          <EmptyState
            icon={<IconLayers className="ico-lg" />}
            title="No workspaces connected"
            action={
              <button className="btn primary" onClick={() => onNavigate("workspaces")}>
                Connect a workspace
              </button>
            }
          >
            Connect a ChatGPT workspace to start routing turns through codex-relay.
          </EmptyState>
        ) : (
          <div className="acct-grid">
            {state.workspaces.map((w) => (
              <AccountCard
                key={w.id}
                w={w}
                now={now}
                isNext={decision.workspace_id === w.id}
                isDefault={w.id === state.default_workspace_id}
              />
            ))}
          </div>
        )}
      </Card>

      <RoutingTimeline state={state} />

      <StatTiles state={state} />

      <CostNote state={state} />

      <Projections state={state} />

      {state.problems.length > 0 && (
        <Card title="Needs attention" icon={<IconAlert className="ico-sm" />} tight>
          <div style={{ margin: "0 -16px" }}>
            {state.problems.map((p, i) => (
              <div className="problem" key={`${p.kind}-${p.workspace_id ?? i}`}>
                <span
                  className={`problem-ico ${p.kind === "credential" || p.kind === "credential_storage" ? "danger" : "warn"}`}
                >
                  <IconAlert className="ico" />
                </span>
                <div className="problem-body">
                  <p>{p.message}</p>
                </div>
                <button
                  className="btn sm"
                  onClick={() => {
                    switch (p.action) {
                      case "reconnect":
                        onReconnect();
                        break;
                      case "open_rules":
                        onNavigate("rules");
                        break;
                      case "open_diagnostics":
                        onNavigate("settings");
                        break;
                      default:
                        onNavigate("workspaces");
                    }
                  }}
                >
                  {p.action_label}
                </button>
              </div>
            ))}
          </div>
        </Card>
      )}

      <Card
        title="Recent activity"
        icon={<IconList className="ico-sm" />}
        actions={
          <button className="btn sm ghost" onClick={() => onNavigate("activity")}>
            See all
          </button>
        }
        tight
      >
        {activity.length === 0 ? (
          <EmptyState icon={<IconInbox className="ico-lg" />} title="No routed turns yet">
            Decisions appear here as Codex sends requests through codex-relay.
          </EmptyState>
        ) : (
          <div style={{ margin: "0 -16px" }}>
            {activity.slice(0, 6).map((row) => (
              <div
                key={row.id}
                style={{ padding: "11px 16px", borderBottom: "1px solid var(--border)" }}
              >
                <div className="row" style={{ gap: 10 }}>
                  <span className="mono" style={{ color: "var(--faint-fg)" }}>
                    {clockText(row.at)}
                  </span>
                  <span className="grow">{row.summary}</span>
                  {row.outcome !== "selected" ? (
                    <Badge kind="danger">blocked</Badge>
                  ) : row.error_class ? (
                    <Badge kind="warn">not delivered</Badge>
                  ) : null}
                </div>
                <Expando summary="Why?">
                  <div className="note">{reasonText(row.reason)}</div>
                  {row.notes.map((n, i) => (
                    <div key={i} style={{ marginTop: 6 }}>
                      <div className="note">{n.message}</div>
                      {n.comparison && <div className="compare">{n.comparison}</div>}
                      {n.rule_id && (
                        <button className="btn link" onClick={() => onEditRule(n.rule_id as string)}>
                          View rule
                        </button>
                      )}
                    </div>
                  ))}
                </Expando>
              </div>
            ))}
          </div>
        )}
      </Card>
    </>
  );
}

/**
 * StatTiles is one horizontally scrollable row, matching the shape of a familiar dashboard.
 *
 * What is deliberately NOT here: an estimated API cost, and a burn projection. An
 * API-equivalent price is not what a subscription charges, and showing one implies a number
 * this tool cannot know. Projections are deferred. Every tile below is a count of something
 * that actually happened.
 */
/**
 * RoutingTimeline answers the question the rest of this page cannot: not "who serves the next
 * turn" but "when does that stop being true".
 *
 * A reserve threshold and a handoff floor are both invisible until the moment they fire. You
 * can read a rule back in plain English and still not know that it changes everything at 24%
 * remaining. This walks one account's quota down and asks the service what it would do at
 * every step, so the boundaries are the engine's own, not a second guess at them.
 */
function RoutingTimeline({ state }: { state: State }) {
  const eligible = state.workspaces.filter((w) => w.windows.length > 0);
  const [workspaceId, setWorkspaceId] = useState("");
  const [sim, setSim] = useState<Simulation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Default to the account that serves now, because that is the one whose exhaustion changes
  // something. Falling back to the first is only for a pool with no decision yet.
  const target =
    workspaceId ||
    (eligible.some((w) => w.id === state.proposed?.workspace_id)
      ? state.proposed!.workspace_id!
      : (eligible[0]?.id ?? ""));

  useEffect(() => {
    if (!target) return;
    let live = true;
    setBusy(true);
    setError(null);
    api
      .simulate(target, 0, "")
      .then((s) => live && setSim(s))
      .catch((e) => live && setError(e instanceof ApiError ? e.message : String(e)))
      .finally(() => live && setBusy(false));
    return () => {
      live = false;
    };
  }, [target, state.version]);

  if (eligible.length === 0) return null;

  return (
    <Card
      title="If this account keeps draining"
      icon={<IconGauge className="ico-sm" />}
      actions={
        eligible.length > 1 ? (
          <select
            className="input sm"
            value={target}
            onChange={(e) => setWorkspaceId(e.target.value)}
            aria-label="Which account to drain"
          >
            {eligible.map((w) => (
              <option key={w.id} value={w.id}>
                {w.name}
              </option>
            ))}
          </select>
        ) : undefined
      }
    >
      {error ? (
        <p className="note">{error}</p>
      ) : !sim || sim.bands.length === 0 ? (
        <p className="note">{busy ? "Working it out…" : "Nothing to simulate yet."}</p>
      ) : (
        <>
          <p className="subline" style={{ marginTop: 0 }}>
            Simulated. {sim.workspace_name}&apos;s {sim.window_label} window from{" "}
            {Math.round(sim.from_percent)}% down to nothing
            {sim.other_workspaces_unchanged ? ", with every other account held where it is" : ""}.
          </p>
          <ol className="timeline">
            {sim.bands.map((b, i) => (
              <li key={i} className={b.outcome === "blocked" ? "tl-blocked" : undefined}>
                <span className="tl-range">
                  {Math.round(b.from_percent)}%
                  {b.to_percent !== b.from_percent ? ` – ${Math.round(b.to_percent)}%` : ""}
                </span>
                <span className="tl-target">
                  {b.workspace_name || (b.outcome === "blocked" ? "nothing can serve" : "—")}
                </span>
                <span className="tl-why">{b.drained_detail || b.summary}</span>
              </li>
            ))}
          </ol>
          <p className="note">
            One account moves; the rest are held at today&apos;s readings, so this shows what the
            rules do, not what next week will look like. A workspace stops being eligible at{" "}
            {sim.handoff_floor_percent}% remaining.
          </p>
        </>
      )}
    </Card>
  );
}

function StatTiles({ state }: { state: State }) {
  const s = state.summary;
  const days = Math.round(s.window_hours / 24);
  const win = days >= 1 ? `${days}d` : `${s.window_hours}h`;

  const tiles = [
    {
      id: "turns",
      label: `Turns (${win})`,
      value: s.turns.toLocaleString(),
      sub: `${s.turns_served} served · ${s.turns_blocked} blocked`,
      icon: <IconList className="ico-sm" />,
      series: s.turns_series,
      color: SERIES_COLORS[0],
    },
    {
      id: "convos",
      label: `Conversations (${win})`,
      value: s.conversations.toLocaleString(),
      sub:
        s.conversations > 0
          ? `${(s.turns / s.conversations).toFixed(1)} turns each`
          : "none yet",
      icon: <IconInbox className="ico-sm" />,
      series: s.turns_series,
      color: SERIES_COLORS[1],
    },
    {
      id: "latency",
      label: `Median first token (${win})`,
      value: s.median_first_token_ms >= 0 ? `${s.median_first_token_ms} ms` : "–",
      sub:
        s.median_first_token_ms >= 0
          ? "measured at this proxy, not a cost"
          : "not measured yet",
      icon: <IconClock className="ico-sm" />,
      series: s.latency_series,
      color: SERIES_COLORS[2],
    },
    {
      id: "errors",
      label: `Error rate (${win})`,
      value: s.turns > 0 ? `${s.error_rate_percent.toFixed(1)}%` : "–",
      sub: s.top_error_class ? `top: ${s.top_error_class}` : "no failures recorded",
      icon: <IconAlert className="ico-sm" />,
      series: s.error_series,
      color: SERIES_COLORS[4],
    },
    {
      id: "tokens",
      label: `Tokens (${win})`,
      value: fmtTokens(s.total_tokens),
      sub:
        s.total_tokens > 0
          ? `${Math.round((s.cached_tokens / Math.max(1, s.input_tokens)) * 100)}% of input cached`
          : "none reported yet",
      icon: <IconGauge className="ico-sm" />,
      series: s.token_series,
      color: SERIES_COLORS[1],
    },
    {
      id: "cost",
      label: `API-equivalent (${win})`,
      value: s.total_tokens > 0 ? `$${s.estimated_cost.toFixed(2)}` : "–",
      sub: "NOT your subscription bill",
      icon: <IconInfo className="ico-sm" />,
      series: s.cost_series,
      color: SERIES_COLORS[3],
    },
    {
      id: "workspaces",
      label: "Workspaces",
      value: String(s.workspaces),
      sub: `${s.rules_active} rule${s.rules_active === 1 ? "" : "s"} active`,
      icon: <IconLayers className="ico-sm" />,
      series: [],
      color: SERIES_COLORS[3],
    },
  ];

  return (
    <div className="tiles" role="group" aria-label="Recent activity summary">
      {tiles.map((t) => (
        <div className="tile" key={t.id}>
          <div className="tile-top">
            <span className="tile-label">{t.label}</span>
            <span className="tile-ico">{t.icon}</span>
          </div>
          <span className="tile-value">{t.value}</span>
          <span className="tile-sub">{t.sub}</span>
          <div className="tile-spark">
            <Sparkline points={t.series} color={t.color} />
          </div>
        </div>
      ))}
    </div>
  );
}

function AccountCard({
  w,
  now,
  isNext,
  isDefault,
}: {
  w: State["workspaces"][number];
  now: Date;
  isNext: boolean;
  isDefault: boolean;
}) {
  return (
    <div className={isNext ? "acct is-next" : "acct"}>
      <div className="acct-head">
        <div style={{ minWidth: 0 }}>
          <div className="acct-name">{w.name}</div>
          <div className="acct-plan">{w.account_id}</div>
        </div>
        <div className="row" style={{ gap: 4 }}>
          {isNext && <Badge kind="next">next</Badge>}
          {isDefault && (
            <Badge kind="neutral" icon={false}>
              <IconPin />
              default
            </Badge>
          )}
          {w.protected && <Badge kind="protect">protected</Badge>}
          {w.paused && <Badge kind="warn">paused</Badge>}
          {!w.credential_ok && <Badge kind="danger">needs sign-in</Badge>}
        </div>
      </div>

      {w.windows.length === 0 ? (
        <p className="note" style={{ margin: 0 }}>
          No quota reading yet. It appears after this workspace serves its first turn.
        </p>
      ) : (
        <div className="acct-windows">
          {w.windows.map((win) => {
            const remaining = Math.max(0, Math.min(100, win.remaining_percent));
            const marker = win.reserve_marker_percent;
            const tone =
              remaining <= 10 ? "crit" : remaining <= 25 || (marker !== undefined && remaining <= marker) ? "low" : "ok";
            return (
              <div key={win.minutes}>
                <div className="acct-win-top">
                  <span className="acct-win-name">{win.label}</span>
                  <span className="acct-win-pct">{pct(remaining)}</span>
                </div>
                <Meter percent={remaining} markerPercent={marker} tone={tone} />
                <div className="acct-win-foot">
                  <IconClock className="ico-sm" />
                  <strong className="acct-win-reset">{resetAtText(win.resets_at, now)}</strong>
                  {marker !== undefined && (
                    <span className="acct-win-threshold">Reserve threshold: {pct(marker)}</span>
                  )}
                  {evidenceBadge(win.evidence, win.observed_at, now)}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function fmtTokens(n: number): string {
  if (n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

/**
 * CostNote is the disclaimer the cost tile requires to be honest.
 *
 * A ChatGPT subscription is a flat fee. The figure above is what the same tokens would cost
 * on the pay-as-you-go API, at rates the user can see and change. It is a sense of scale, not
 * a bill, and saying so is the condition for showing it at all.
 */
function CostNote({ state }: { state: State }) {
  const s = state.summary;
  if (s.total_tokens <= 0) return null;
  const partial = s.turns_with_usage < s.turns;
  return (
    <div className="callout">
      <IconInfo className="ico" />
      <div>
        <strong>The API-equivalent figure is not what you are charged.</strong> Your ChatGPT
        plan is a flat subscription. ${s.estimated_cost.toFixed(2)} is what these{" "}
        {fmtTokens(s.total_tokens)} tokens would have cost on the pay-as-you-go API at{" "}
        ${s.pricing.input_per_mtok}/M input, ${s.pricing.cached_per_mtok}/M cached and{" "}
        ${s.pricing.output_per_mtok}/M output
        {s.pricing.user_set ? " (your rates)" : " (default rates, which you can change in Settings)"}.
        {partial && (
          <>
            {" "}
            Only {s.turns_with_usage} of {s.turns} turns reported token counts, so this covers
            part of the window.
          </>
        )}
      </div>
    </div>
  );
}

/**
 * Projections extrapolate the rate already observed inside each window.
 *
 * Every row shows its own basis and confidence, because the method is a straight line and its
 * weakness should be as visible as its answer. Confidence is never "high".
 */
function Projections({ state }: { state: State }) {
  const rows = state.projections.filter((p) => p.confidence !== "none");
  const idle = state.projections.filter((p) => p.confidence === "none");
  if (state.projections.length === 0) return null;

  return (
    <Card title="Burn projection" icon={<IconClock className="ico-sm" />}>
      {rows.length === 0 ? (
        <p className="note" style={{ margin: 0 }}>
          Nothing is being consumed fast enough to project yet.{" "}
          {idle[0]?.basis}
        </p>
      ) : (
        <div className="stack">
          {rows.map((p) => (
            <div key={`${p.workspace_id}-${p.window_minutes}`} className="proj">
              <div className="spread" style={{ alignItems: "baseline" }}>
                <div className="grow">
                  <span className="cell-strong">{p.workspace_name}</span>{" "}
                  <span className="note">{p.window_label}</span>
                </div>
                <Badge kind={p.will_run_out_before_reset ? "danger" : "ok"}>
                  {p.will_run_out_before_reset ? "runs out first" : "lasts to reset"}
                </Badge>
                <Badge kind={p.confidence === "moderate" ? "neutral" : "warn"}>
                  {p.confidence} confidence
                </Badge>
              </div>
              <p className="note" style={{ margin: "6px 0 0" }}>
                Using {p.burn_percent_per_hour.toFixed(1)}% per hour.{" "}
                {p.hours_to_empty >= 0
                  ? `At that rate the remaining ${p.remaining_percent.toFixed(0)}% lasts about ${fmtHours(p.hours_to_empty)}, and this window resets in ${fmtHours(p.hours_to_reset)}.`
                  : `This window resets in ${fmtHours(p.hours_to_reset)}.`}
              </p>
              <p className="note" style={{ margin: "4px 0 0", fontStyle: "italic" }}>
                {p.basis}
              </p>
            </div>
          ))}
          <p className="note" style={{ margin: 0 }}>
            These are straight-line extrapolations of usage already observed in each window.
            They assume the next few hours look like the last few, and nothing else. They are
            not a forecast of how you work, and codex-relay never routes on them: routing uses
            the rules you wrote, on readings that already happened.
          </p>
        </div>
      )}
    </Card>
  );
}

function fmtHours(h: number): string {
  if (h >= 24) return `${(h / 24).toFixed(1)} days`;
  if (h < 1) return `${Math.round(h * 60)} min`;
  return `${h.toFixed(1)} h`;
}
