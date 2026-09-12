// Rules: structured fields only. The English sentence is generated from the fields and is
// never parsed back, and the preview beside it runs the service's own evaluator.

import { useEffect, useMemo, useState } from "react";
import type { Decision, Rule, RuleKind, Scenario, State } from "./api";
import { ApiError, api } from "./api";
import { Badge, ErrorBox, Expando, SimBadge, reasonText } from "./ui";
import {
  IconFlask,
  IconGauge,
  IconPencil,
  IconShield,
  IconShieldCheck,
  IconTrash,
} from "./icons";

const COMMON_WINDOWS = [300, 10080];

export function humanWindow(minutes: number): string {
  if (minutes <= 0) return "unknown window";
  if (minutes % 10080 === 0 && minutes / 10080 === 1) return "weekly";
  if (minutes % 1440 === 0) {
    const d = minutes / 1440;
    return d === 1 ? "daily" : `${d}-day`;
  }
  if (minutes % 60 === 0) return `${minutes / 60}-hour`;
  return `${minutes}-minute`;
}

function humanHours(h: number): string {
  if (h >= 24 && Number.isInteger(h) && h % 24 === 0) {
    const d = h / 24;
    return d === 1 ? "1 day" : `${d} days`;
  }
  if (h === 1) return "1 hour";
  return `${h} hours`;
}

const cmpSymbol = (c: Rule["comparison"]) => (c === "below" ? "<" : "<=");
const resetSymbol = (c: Rule["reset_comparison"]) => (c === "at_most" ? "<=" : ">=");

/**
 * sentenceOf mirrors the service's own sentence generator so the editor can show the
 * wording before anything is saved. After a save the service's returned sentence replaces
 * this one, so the two can never quietly disagree.
 */
export function sentenceOf(r: Rule, name: (id: string) => string): string {
  if (r.kind === "prefer") return `Prefer ${name(r.source_workspace_id)} for new conversations.`;
  const tail =
    r.no_alternative === "use_protected"
      ? `use ${name(r.source_workspace_id)} anyway`
      : "stop and explain";
  return (
    `Protect ${name(r.source_workspace_id)} and prefer ${name(r.preferred_workspace_id)} when its ` +
    `${humanWindow(r.window.minutes)} quota remaining is ${cmpSymbol(r.comparison)} ${r.remaining_percent}% ` +
    `and its reset is ${resetSymbol(r.reset_comparison)} ${humanHours(r.reset_hours)} away. ` +
    `If ${name(r.preferred_workspace_id)} cannot serve the request, ${tail}.`
  );
}

export function blankRule(kind: RuleKind, sourceID: string, altID: string): Rule {
  return {
    id: "",
    kind,
    enabled: true,
    priority: 0,
    source_workspace_id: sourceID,
    window: { minutes: 10080 },
    comparison: "at_or_below",
    remaining_percent: 30,
    reset_comparison: "at_least",
    reset_hours: 96,
    preferred_workspace_id: altID,
    no_alternative: "stop_and_explain",
  };
}

function message(e: unknown): string {
  return e instanceof ApiError ? e.message : String(e);
}

export function Rules({
  state,
  reload,
  draft,
  setDraft,
}: {
  state: State;
  reload: () => void;
  draft: Rule | null;
  setDraft: (r: Rule | null) => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<{ id: string; version: number; sentence: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [preview, setPreview] = useState<Decision | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [model, setModel] = useState("");
  const [useScenario, setUseScenario] = useState(false);
  const [scRemaining, setScRemaining] = useState(28);
  const [scResetDays, setScResetDays] = useState(5);

  const nameOf = (id: string) => state.workspaces.find((w) => w.id === id)?.name ?? id;
  const others = draft ? state.workspaces.filter((w) => w.id !== draft.source_workspace_id) : [];

  const windowChoices = useMemo(() => {
    const set = new Set<number>(COMMON_WINDOWS);
    for (const w of state.workspaces) for (const win of w.windows) set.add(win.minutes);
    if (draft) set.add(draft.window.minutes);
    return [...set].filter((m) => m > 0).sort((a, b) => a - b);
  }, [state.workspaces, draft]);

  // A change to the fields invalidates any previous preview and any saved marker: the
  // dashboard must never show a stale "applied" state beside edited fields.
  useEffect(() => {
    setPreview(null);
    setPreviewError(null);
    setSaved(null);
  }, [draft]);

  const patch = (p: Partial<Rule>) => {
    if (draft) setDraft({ ...draft, ...p });
  };

  const candidateRules = (): Rule[] => {
    if (!draft) return state.rules;
    const others = state.rules.filter((r) => r.id !== draft.id || draft.id === "");
    return [...others.map((r) => ({ ...r })), draft];
  };

  const runPreview = async () => {
    if (!draft) return;
    setPreviewing(true);
    setPreviewError(null);
    try {
      const scenarios: Scenario[] = useScenario
        ? [
            {
              workspace_id: draft.source_workspace_id,
              window_minutes: draft.window.minutes,
              remaining_percent: scRemaining,
              reset_in_hours: scResetDays * 24,
            },
          ]
        : [];
      const r = await api.preview(candidateRules(), model.trim(), scenarios);
      setPreview(r.decision);
    } catch (e) {
      setPreview(null);
      setPreviewError(message(e));
    } finally {
      setPreviewing(false);
    }
  };

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.saveRule(draft);
      setSaved({ id: r.id, version: r.state_version, sentence: r.sentence });
      setDraft({ ...draft, id: r.id });
      reload();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    setBusy(true);
    setError(null);
    try {
      await api.deleteRule(id);
      if (draft?.id === id) setDraft(null);
      reload();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  };

  const toggle = async (r: Rule) => {
    setBusy(true);
    setError(null);
    try {
      await api.saveRule({ ...r, enabled: !r.enabled });
      reload();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  };

  const first = state.workspaces[0]?.id ?? "";
  const second = state.workspaces[1]?.id ?? "";

  return (
    <>
      <section className="card">
        <div className="card-head">
          <div>
            <p className="card-title" style={{ fontSize: 13 }}>
              <IconShield className="ico-sm" />
              Rules
            </p>
            <p className="subline">
              Two templates. Preference orders otherwise eligible workspaces; protection removes one
              from consideration while its condition matches.
            </p>
          </div>
          <div className="row">
            <button
              className="btn"
              disabled={state.workspaces.length === 0}
              onClick={() => setDraft(blankRule("prefer", first, ""))}
            >
              Prefer a workspace
            </button>
            <button
              className="btn primary"
              disabled={state.workspaces.length < 2}
              title={state.workspaces.length < 2 ? "A reserve rule needs a second workspace to prefer instead." : undefined}
              onClick={() => setDraft(blankRule("reserve", first, second))}
            >
              Protect a quota reserve
            </button>
          </div>
        </div>
        {state.workspaces.length < 2 && (
          <p className="note" style={{ marginTop: 10 }}>
            A reserve rule needs at least two workspaces: one to protect and one to prefer instead.
          </p>
        )}
        <ErrorBox error={error} />
      </section>

      {draft && (
        <section className="card">
          <div className="card-head">
            <h2 className="card-title">
              <IconPencil className="ico-sm" />
              {draft.id ? "Edit rule" : "New rule"}
            </h2>
          </div>
          <div className="card-body">

          {draft.kind === "prefer" ? (
            <div className="field" style={{ maxWidth: 320 }}>
              <label htmlFor="r-source">Workspace to prefer</label>
              <select
                id="r-source"
                value={draft.source_workspace_id}
                onChange={(e) => patch({ source_workspace_id: e.target.value })}
              >
                {state.workspaces.map((w) => (
                  <option key={w.id} value={w.id}>
                    {w.name}
                  </option>
                ))}
              </select>
              <span className="hint">
                Preference only orders choices that are already eligible. It never overrides protection.
              </span>
            </div>
          ) : (
            <>
              <div className="inline-fields" style={{ marginBottom: 14 }}>
                <div className="field" style={{ minWidth: 200 }}>
                  <label htmlFor="r-source">Protect this workspace</label>
                  <select
                    id="r-source"
                    value={draft.source_workspace_id}
                    onChange={(e) => {
                      const v = e.target.value;
                      patch({
                        source_workspace_id: v,
                        preferred_workspace_id:
                          draft.preferred_workspace_id === v
                            ? state.workspaces.find((w) => w.id !== v)?.id ?? ""
                            : draft.preferred_workspace_id,
                      });
                    }}
                  >
                    {state.workspaces.map((w) => (
                      <option key={w.id} value={w.id}>
                        {w.name}
                      </option>
                    ))}
                  </select>
                </div>
                <div className="field" style={{ minWidth: 170 }}>
                  <label htmlFor="r-window">Quota window</label>
                  <select
                    id="r-window"
                    value={draft.window.minutes}
                    onChange={(e) => patch({ window: { minutes: Number(e.target.value) } })}
                  >
                    {windowChoices.map((m) => (
                      <option key={m} value={m}>
                        {humanWindow(m)} ({m} min)
                      </option>
                    ))}
                  </select>
                  <span className="hint">
                    Codex reports windows by duration, not by name. A window this workspace has not
                    reported evaluates as unknown, not as clear.
                  </span>
                </div>
              </div>

              <div className="inline-fields" style={{ marginBottom: 14 }}>
                <div className="field" style={{ minWidth: 150 }}>
                  <label htmlFor="r-cmp">Remaining quota is</label>
                  <select
                    id="r-cmp"
                    value={draft.comparison}
                    onChange={(e) => patch({ comparison: e.target.value as Rule["comparison"] })}
                  >
                    <option value="at_or_below">at or below (&le;)</option>
                    <option value="below">below (&lt;)</option>
                  </select>
                </div>
                <div className="field" style={{ maxWidth: 110 }}>
                  <label htmlFor="r-pct">Percent remaining</label>
                  <input
                    id="r-pct"
                    type="number"
                    min={0}
                    max={100}
                    step={1}
                    value={draft.remaining_percent}
                    onChange={(e) => patch({ remaining_percent: Number(e.target.value) })}
                  />
                </div>
                <div className="field" style={{ minWidth: 150 }}>
                  <label htmlFor="r-rcmp">And its reset is</label>
                  <select
                    id="r-rcmp"
                    value={draft.reset_comparison}
                    onChange={(e) => patch({ reset_comparison: e.target.value as Rule["reset_comparison"] })}
                  >
                    <option value="at_least">at least (&ge;)</option>
                    <option value="at_most">at most (&le;)</option>
                  </select>
                </div>
                <div className="field" style={{ maxWidth: 120 }}>
                  <label htmlFor="r-hours">Hours away</label>
                  <input
                    id="r-hours"
                    type="number"
                    min={0}
                    step={1}
                    value={draft.reset_hours}
                    onChange={(e) => patch({ reset_hours: Number(e.target.value) })}
                  />
                  <span className="hint">{humanHours(draft.reset_hours)}</span>
                </div>
              </div>

              <div className="inline-fields" style={{ marginBottom: 14 }}>
                <div className="field" style={{ minWidth: 200 }}>
                  <label htmlFor="r-alt">Prefer this instead</label>
                  <select
                    id="r-alt"
                    value={draft.preferred_workspace_id}
                    onChange={(e) => patch({ preferred_workspace_id: e.target.value })}
                  >
                    <option value="">Choose a workspace</option>
                    {others.map((w) => (
                      <option key={w.id} value={w.id}>
                        {w.name}
                      </option>
                    ))}
                  </select>
                </div>
                <div className="field" style={{ minWidth: 280 }}>
                  <label htmlFor="r-noalt">When no alternative is eligible</label>
                  <select
                    id="r-noalt"
                    value={draft.no_alternative}
                    onChange={(e) => patch({ no_alternative: e.target.value as Rule["no_alternative"] })}
                  >
                    <option value="stop_and_explain">Stop and explain (nothing is spent)</option>
                    <option value="use_protected">Use the protected workspace anyway</option>
                  </select>
                  <span className="hint">
                    Using the protected workspace anyway spends the quota this rule exists to reserve.
                  </span>
                </div>
              </div>

              <div className="compare" style={{ marginBottom: 14 }}>
                {`remaining_percent ${cmpSymbol(draft.comparison)} ${draft.remaining_percent}`}
                {"  AND  "}
                {`(reset_at - now) ${resetSymbol(draft.reset_comparison)} ${draft.reset_hours}h`}
                {"\nBoth conditions must match. Remaining is 100 - used_percent; the reset comparison is an exact duration, not a calendar day count."}
              </div>
            </>
          )}

          <div className="field">
            <label>This rule reads</label>
            <p className="sentence">
              <IconShield className="ico" />
              <span>{sentenceOf(draft, nameOf)}</span>
            </p>
          </div>

          <div className="row" style={{ marginBottom: 12 }}>
            <label className="row" style={{ gap: 6 }}>
              <input type="checkbox" checked={draft.enabled} onChange={(e) => patch({ enabled: e.target.checked })} />
              <span>Enabled</span>
            </label>
          </div>

          <div className="actions">
            <button className="btn primary" onClick={save} disabled={busy}>
              {busy ? "Saving…" : draft.id ? "Save changes" : "Save rule"}
            </button>
            <button className="btn" onClick={() => setDraft(null)} disabled={busy}>
              Close
            </button>
            {saved ? (
              <span className="row" style={{ gap: 6 }}>
                <Badge kind="ok">saved and applied</Badge>
                <span className="note">state version {saved.version}</span>
              </span>
            ) : (
              <span className="note">Not saved yet. Nothing changes until you save.</span>
            )}
          </div>
          {saved && saved.sentence !== sentenceOf(draft, nameOf) && (
            <p className="note" style={{ marginTop: 8 }}>
              The service recorded it as: {saved.sentence}
            </p>
          )}

          <hr style={{ border: 0, borderTop: "1px solid var(--border)", margin: "18px 0 14px" }} />

          <h3 className="card-title" style={{ marginTop: 18, marginBottom: 10 }}>
            <IconGauge className="ico-sm" />
            Preview
          </h3>
          <p className="note" style={{ marginTop: 0 }}>
            Preview runs the same evaluator the proxy uses, against the same published state. It
            consumes no model quota and cannot change quota or conversation ownership.
          </p>

          <div className="inline-fields" style={{ marginBottom: 12 }}>
            <div className="field" style={{ minWidth: 170 }}>
              <label htmlFor="p-model">For model (optional)</label>
              <input id="p-model" type="text" value={model} placeholder="any model" onChange={(e) => setModel(e.target.value)} />
            </div>
            <div className="field">
              <label className="row" style={{ gap: 6, fontWeight: 400 }}>
                <input type="checkbox" checked={useScenario} onChange={(e) => setUseScenario(e.target.checked)} />
                <IconFlask className="ico-sm" />
                <span>Try a scenario instead of current data</span>
              </label>
            </div>
          </div>

          {useScenario && draft.kind === "reserve" && (
            <div className="inline-fields" style={{ marginBottom: 12 }}>
              <div className="field" style={{ maxWidth: 150 }}>
                <label htmlFor="s-pct">{nameOf(draft.source_workspace_id)} remaining</label>
                <input
                  id="s-pct"
                  type="number"
                  min={0}
                  max={100}
                  value={scRemaining}
                  onChange={(e) => setScRemaining(Number(e.target.value))}
                />
              </div>
              <div className="field" style={{ maxWidth: 150 }}>
                <label htmlFor="s-days">Days until reset</label>
                <input
                  id="s-days"
                  type="number"
                  min={0}
                  step={0.5}
                  value={scResetDays}
                  onChange={(e) => setScResetDays(Number(e.target.value))}
                />
                <span className="hint">{scResetDays * 24} hours</span>
              </div>
            </div>
          )}
          {useScenario && draft.kind === "prefer" && (
            <p className="note">A preference rule has no quota condition, so a scenario changes nothing for it.</p>
          )}

          <div className="actions" style={{ marginTop: 0 }}>
            <button className="btn" onClick={runPreview} disabled={previewing}>
              {previewing ? "Evaluating…" : "Preview this rule"}
            </button>
          </div>

          <ErrorBox error={previewError} />

          {preview && (
            <div style={{ marginTop: 12 }}>
              <div className="row" style={{ marginBottom: 6 }}>
                {preview.simulated ? <SimBadge /> : <Badge kind="neutral">current data</Badge>}
                {preview.outcome === "blocked" && <Badge kind="danger">blocked</Badge>}
                {preview.unknown_evidence && <Badge kind="warn">evidence unknown</Badge>}
              </div>
              <p className="sentence">
                <IconGauge className="ico" />
                <span>{preview.summary}</span>
              </p>
              <div style={{ marginTop: 10 }}>
                {preview.candidates.map((c) => (
                  <div key={c.workspace_id} className="row" style={{ gap: 8, padding: "3px 0" }}>
                    <strong style={{ minWidth: 150 }}>{nameOf(c.workspace_id)}</strong>
                    {c.eligible ? (
                      <Badge kind="ok">eligible{c.rank ? ` (rank ${c.rank})` : ""}</Badge>
                    ) : (
                      <Badge kind="warn">{reasonText(c.reason)}</Badge>
                    )}
                    {c.detail && <span className="note">{c.detail}</span>}
                  </div>
                ))}
              </div>
              {preview.notes.map((n, i) => (
                <div key={i} style={{ marginTop: 8 }}>
                  <div className="note">{n.message}</div>
                  {n.comparison && <div className="compare">{n.comparison}</div>}
                </div>
              ))}
              <p className="note" style={{ marginTop: 10 }}>
                This is a point-in-time result at state version {preview.state_version}. It is not a
                reservation of future capacity.
              </p>
            </div>
          )}
          </div>
        </section>
      )}

      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconShieldCheck className="ico-sm" />
            Saved rules
          </h2>
        </div>
        <div className="card-body">
        {state.rules.length === 0 ? (
          <div className="empty">No rules yet. Every workspace that is connected and not paused is eligible.</div>
        ) : (
          state.rules.map((r) => (
            <div className="problem" key={r.id}>
              <div style={{ minWidth: 0 }}>
                <div className="row" style={{ gap: 7, marginBottom: 3 }}>
                  <Badge kind={r.kind === "reserve" ? "protect" : "neutral"}>{r.kind}</Badge>
                  {r.enabled ? <Badge kind="ok">on</Badge> : <Badge kind="neutral">off</Badge>}
                </div>
                <p className="sentence">
                  <IconShield className="ico" />
                  <span>{r.sentence}</span>
                </p>
                <Expando summary="Exact comparison">
                  <div className="compare">
                    {r.kind === "reserve"
                      ? `remaining_percent ${cmpSymbol(r.comparison)} ${r.remaining_percent}  AND  (reset_at - now) ${resetSymbol(
                          r.reset_comparison,
                        )} ${r.reset_hours}h  on the ${humanWindow(r.window.minutes)} window`
                      : "No condition: preference orders eligible workspaces only."}
                  </div>
                </Expando>
              </div>
              <div className="row">
                <button className="btn" onClick={() => setDraft({ ...r })} disabled={busy}>
                  Edit
                </button>
                <button className="btn" onClick={() => toggle(r)} disabled={busy}>
                  {r.enabled ? "Turn off" : "Turn on"}
                </button>
                <button className="btn sm danger" onClick={() => remove(r.id)} disabled={busy}>
                  <IconTrash className="ico-sm" />
                  Delete
                </button>
              </div>
            </div>
          ))
        )}
        </div>
      </section>
    </>
  );
}
