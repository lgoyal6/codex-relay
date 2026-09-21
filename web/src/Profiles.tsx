import { useEffect, useMemo, useState } from "react";
import type { RoutingProfile, State } from "./api";
import { ApiError, api } from "./api";
import { Badge, ErrorBox, OkBox, pct } from "./ui";
import { IconList, IconPencil, IconPlus, IconTrash } from "./icons";

function message(e: unknown): string {
  return e instanceof ApiError ? e.message : String(e);
}

function uniqueCommand(base: string, state: State): string {
  const used = new Set(state.profiles.flatMap((p) => [p.command, ...p.aliases]));
  const maxLength = 32;
  let candidate = base.slice(0, maxLength);
  let n = 2;
  while (used.has(candidate)) {
    const suffix = `-${n++}`;
    candidate = `${base.slice(0, maxLength - suffix.length)}${suffix}`;
  }
  return candidate;
}

function blankProfile(state: State): RoutingProfile {
  const ids = state.workspaces.map((w) => w.id);
  return {
    id: "",
    name: "New profile",
    command: uniqueCommand("new-profile", state),
    aliases: [],
    mode: "priority",
    priority_workspace_ids: ids,
    pace_workspace_ids: [],
    overflow_workspace_id: ids[0] ?? "",
    target_remaining_percent: 3,
    default_workspace_id: ids[0] ?? "",
    handoff_below_percent: 2,
    disabled_workspace_ids: [],
    subagent_helper_enabled: false,
    subagent_helper_workspace_id: ids[0] ?? "",
    subagent_helper_model: "gpt-5.6-luna",
  };
}

function profileSentence(profile: RoutingProfile, nameOf: (id: string) => string): string {
  const helper = profile.subagent_helper_enabled
    ? ` Delegated ${profile.subagent_helper_model || "Luna"} subagents prefer ${nameOf(profile.subagent_helper_workspace_id ?? "")} and fall back to this profile when unavailable.`
    : "";
  if (profile.mode === "pace") {
    const paced = profile.pace_workspace_ids.map(nameOf).join(", ") || "no workspaces";
    const overflow = nameOf(profile.overflow_workspace_id ?? "") || "no overflow workspace";
    return `Pace ${paced} toward ${profile.target_remaining_percent}% remaining at each weekly reset. Use ${overflow} as overflow while they are on schedule.${helper}`;
  }
  const ordered = profile.priority_workspace_ids.map(nameOf).join(" -> ") || "no workspaces";
  return `Try workspaces in this order: ${ordered}.${helper}`;
}

function OrderedWorkspaces({
  ids,
  state,
  onChange,
}: {
  ids: string[];
  state: State;
  onChange: (ids: string[]) => void;
}) {
  const available = state.workspaces.filter((w) => !ids.includes(w.id));
  const move = (index: number, delta: number) => {
    const next = [...ids];
    const other = index + delta;
    if (other < 0 || other >= next.length) return;
    [next[index], next[other]] = [next[other], next[index]];
    onChange(next);
  };

  return (
    <div className="stack" style={{ gap: 7 }}>
      {ids.map((id, index) => {
        const workspace = state.workspaces.find((w) => w.id === id);
        return (
          <div className="row profile-order-row" key={id}>
            <span className="mono" style={{ minWidth: 22 }}>{index + 1}</span>
            <span className="profile-order-name">{workspace?.name ?? id}</span>
            <button className="btn sm" type="button" disabled={index === 0} onClick={() => move(index, -1)}>
              Up
            </button>
            <button className="btn sm" type="button" disabled={index === ids.length - 1} onClick={() => move(index, 1)}>
              Down
            </button>
            <button className="btn sm" type="button" onClick={() => onChange(ids.filter((x) => x !== id))}>
              Remove
            </button>
          </div>
        );
      })}
      {available.length > 0 && (
        <div className="field" style={{ maxWidth: 300 }}>
          <label htmlFor="profile-add-priority">Add workspace to the order</label>
          <select
            id="profile-add-priority"
            value=""
            onChange={(e) => e.target.value && onChange([...ids, e.target.value])}
          >
            <option value="">Choose a workspace</option>
            {available.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
          </select>
        </div>
      )}
    </div>
  );
}

export function Profiles({ state, reload }: { state: State; reload: () => void }) {
  const [draft, setDraft] = useState<RoutingProfile | null>(null);
  const [aliases, setAliases] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [ok, setOK] = useState<string | null>(null);

  const nameOf = (id: string) => state.workspaces.find((w) => w.id === id)?.name ?? id;
  const active = state.profiles.find((p) => p.id === state.active_profile_id);

  useEffect(() => {
    setAliases(draft?.aliases.join(", ") ?? "");
    setError(null);
    setOK(null);
  }, [draft?.id]);

  const patch = (value: Partial<RoutingProfile>) => {
    setDraft((current) => current ? { ...current, ...value } : current);
    setOK(null);
  };

  const execute = async (label: string, action: () => Promise<void>) => {
    setBusy(label);
    setError(null);
    setOK(null);
    try {
      await action();
      reload();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(null);
    }
  };

  const save = () => execute("save", async () => {
    if (!draft) return;
    const candidate = {
      ...draft,
      aliases: aliases.split(",").map((x) => x.trim()).filter(Boolean),
    };
    const result = await api.saveProfile(candidate);
    setDraft(result.profile);
    setAliases(result.profile.aliases.join(", "));
    setOK(`Saved ${result.profile.name}. Run relaypool ${result.profile.command} to activate it.`);
  });

  const duplicate = (profile: RoutingProfile) => {
    const command = uniqueCommand(`${profile.command}-copy`, state);
    setDraft({
      ...profile,
      id: "",
      name: `${profile.name} copy`,
      command,
      aliases: [],
      created_at: undefined,
      updated_at: undefined,
    });
  };

  const paceRows = useMemo(
    () => (state.pace_standings ?? []).map((standing) => ({ ...standing, name: nameOf(standing.workspace_id) })),
    [state.pace_standings, state.workspaces],
  );

  return (
    <>
      <section className="card">
        <div className="card-head">
          <div>
            <p className="card-title" style={{ fontSize: 13 }}>
              <IconList className="ico-sm" />
              Routing profiles
            </p>
            <p className="subline">
              Save complete routing strategies and give each one your own relaypool command.
              Activating a profile affects the next turn, including an existing conversation.
            </p>
          </div>
          <button
            className="btn primary"
            disabled={state.workspaces.length === 0}
            onClick={() => setDraft(blankProfile(state))}
          >
            <IconPlus className="ico-sm" />
            New profile
          </button>
        </div>
        <div className="card-body">
          {active ? (
            <p className="note" style={{ margin: 0 }}>
              Active: <strong>{active.name}</strong> via <code>relaypool {active.command}</code>. {active.sentence}
            </p>
          ) : (
            <p className="note" style={{ margin: 0 }}>
              No profile is active. Existing rules and the default workspace still control routing.
            </p>
          )}
          <ErrorBox error={error} />
          <OkBox message={ok} />
        </div>
      </section>

      {active?.mode === "pace" && (
        <section className="card">
          <div className="card-head">
            <h2 className="card-title">Weekly pace now</h2>
          </div>
          <div className="card-body">
            <div className="table-wrap">
              <table className="grid">
                <thead><tr><th>Workspace</th><th>Now</th><th>Target now</th><th>Difference</th><th>Status</th></tr></thead>
                <tbody>
                  {paceRows.map((row) => (
                    <tr key={row.workspace_id}>
                      <td>{row.name}</td>
                      {row.known ? (
                        <>
                          <td>{pct(row.remaining_percent)}</td>
                          <td>{pct(row.expected_percent)}</td>
                          <td>{row.delta_percent > 0 ? "+" : ""}{pct(row.delta_percent)}</td>
                        </>
                      ) : <td colSpan={3}>No weekly quota evidence</td>}
                      <td>{row.status}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </section>
      )}

      {draft && (
        <section className="card">
          <div className="card-head">
            <h2 className="card-title">
              <IconPencil className="ico-sm" />
              {draft.id ? "Edit profile" : "New profile"}
            </h2>
          </div>
          <div className="card-body">
            <div className="inline-fields" style={{ marginBottom: 14 }}>
              <div className="field" style={{ minWidth: 220 }}>
                <label htmlFor="profile-name">Name</label>
                <input id="profile-name" required value={draft.name} onChange={(e) => patch({ name: e.target.value })} />
              </div>
              <div className="field" style={{ minWidth: 180 }}>
                <label htmlFor="profile-command">Command</label>
                <input id="profile-command" className="mono" required value={draft.command} onChange={(e) => patch({ command: e.target.value })} />
                <span className="hint">relaypool {draft.command || "command"}</span>
              </div>
              <div className="field" style={{ minWidth: 240 }}>
                <label htmlFor="profile-aliases">Aliases, comma separated</label>
                <input id="profile-aliases" className="mono" value={aliases} onChange={(e) => setAliases(e.target.value)} />
              </div>
              <div className="field" style={{ minWidth: 150 }}>
                <label htmlFor="profile-mode">Routing mode</label>
                <select id="profile-mode" value={draft.mode} onChange={(e) => patch({ mode: e.target.value as RoutingProfile["mode"] })}>
                  <option value="priority">Fixed priority</option>
                  <option value="pace">Weekly pace</option>
                </select>
              </div>
            </div>

            <div className="field" style={{ marginBottom: 18 }}>
              <label>Fallback order</label>
              <OrderedWorkspaces
                ids={draft.priority_workspace_ids}
                state={state}
                onChange={(ids) => patch({ priority_workspace_ids: ids })}
              />
              <span className="hint">The first eligible workspace wins. Pause, credentials, model support, quota exhaustion, and reserve rules still apply.</span>
            </div>

            {draft.mode === "pace" && (
              <div className="compare" style={{ marginBottom: 18 }}>
                <div className="field" style={{ marginBottom: 12 }}>
                  <label>Workspaces to pace toward their weekly reset</label>
                  <div className="stack" style={{ gap: 6 }}>
                    {state.workspaces.map((w) => (
                      <label key={w.id} className="row">
                        <input
                          type="checkbox"
                          checked={draft.pace_workspace_ids.includes(w.id)}
                          onChange={(e) => patch({
                            pace_workspace_ids: e.target.checked
                              ? [...draft.pace_workspace_ids, w.id]
                              : draft.pace_workspace_ids.filter((id) => id !== w.id),
                          })}
                        />
                        {w.name}
                      </label>
                    ))}
                  </div>
                </div>
                <div className="inline-fields">
                  <div className="field" style={{ minWidth: 220 }}>
                    <label htmlFor="profile-overflow">Overflow workspace</label>
                    <select id="profile-overflow" value={draft.overflow_workspace_id ?? ""} onChange={(e) => patch({ overflow_workspace_id: e.target.value })}>
                      <option value="">Choose a workspace</option>
                      {state.workspaces.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
                    </select>
                  </div>
                  <div className="field" style={{ maxWidth: 190 }}>
                    <label htmlFor="profile-target">Desired quota at reset</label>
                    <input id="profile-target" type="number" min={0} max={100} step={1} value={draft.target_remaining_percent} onChange={(e) => patch({ target_remaining_percent: Number(e.target.value) })} />
                    <span className="hint">Percent remaining. The default is 3%.</span>
                  </div>
                </div>
                <p className="note" style={{ marginBottom: 0 }}>
                  The relay evaluates a linear target on every turn. A paced workspace above that target needs usage; when all are on schedule, overflow wins.
                </p>
              </div>
            )}

            <div className="inline-fields" style={{ marginBottom: 18 }}>
              <div className="field" style={{ minWidth: 220 }}>
                <label htmlFor="profile-default">Default workspace</label>
                <select id="profile-default" value={draft.default_workspace_id ?? ""} onChange={(e) => patch({ default_workspace_id: e.target.value })}>
                  <option value="">Use the global default</option>
                  {state.workspaces.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
                </select>
              </div>
              <div className="field" style={{ maxWidth: 200 }}>
                <label htmlFor="profile-handoff">Conversation handoff floor</label>
                <input id="profile-handoff" type="number" min={0} max={100} step={0.5} value={draft.handoff_below_percent} onChange={(e) => patch({ handoff_below_percent: Number(e.target.value) })} />
                <span className="hint">Percent remaining. Zero disables threshold handoff.</span>
              </div>
            </div>

            <div className="compare" style={{ marginBottom: 18 }}>
              <label className="row" style={{ alignItems: "flex-start" }}>
                <input
                  type="checkbox"
                  checked={draft.subagent_helper_enabled}
                  onChange={(e) => patch({ subagent_helper_enabled: e.target.checked })}
                />
                <span>
                  <strong>Use a dedicated Luna workspace for delegated helpers</strong>
                  <span className="hint" style={{ display: "block", marginTop: 3 }}>
                    Parent tasks keep this profile's normal Sol routing. Only requests Codex marks as subagents use this preference.
                  </span>
                </span>
              </label>
              {draft.subagent_helper_enabled && (
                <div className="inline-fields" style={{ marginTop: 14 }}>
                  <div className="field" style={{ minWidth: 220 }}>
                    <label htmlFor="profile-helper-workspace">Helper workspace</label>
                    <select
                      id="profile-helper-workspace"
                      required
                      value={draft.subagent_helper_workspace_id ?? ""}
                      onChange={(e) => patch({ subagent_helper_workspace_id: e.target.value })}
                    >
                      <option value="">Choose a workspace</option>
                      {state.workspaces.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
                    </select>
                    <span className="hint">If it cannot serve Luna, the normal profile order is used.</span>
                  </div>
                  <div className="field" style={{ minWidth: 190 }}>
                    <label htmlFor="profile-helper-model">Helper model</label>
                    <input
                      id="profile-helper-model"
                      className="mono"
                      required
                      value={draft.subagent_helper_model ?? ""}
                      onChange={(e) => patch({ subagent_helper_model: e.target.value })}
                    />
                    <span className="hint">Setup configures Codex to use gpt-5.6-luna by default.</span>
                  </div>
                </div>
              )}
            </div>

            <div className="field" style={{ marginBottom: 18 }}>
              <label>Disable inside this profile</label>
              <div className="stack" style={{ gap: 6 }}>
                {state.workspaces.map((w) => (
                  <label key={w.id} className="row">
                    <input
                      type="checkbox"
                      checked={draft.disabled_workspace_ids.includes(w.id)}
                      onChange={(e) => patch({
                        disabled_workspace_ids: e.target.checked
                          ? [...draft.disabled_workspace_ids, w.id]
                          : draft.disabled_workspace_ids.filter((id) => id !== w.id),
                      })}
                    />
                    {w.name}
                  </label>
                ))}
              </div>
            </div>

            <div className="compare" style={{ marginBottom: 14 }}>
              <strong>What this profile does</strong>
              <div style={{ marginTop: 6 }}>{profileSentence(draft, nameOf)}</div>
            </div>
            <div className="actions">
              <button className="btn primary" disabled={busy !== null} onClick={save}>{busy === "save" ? "Saving..." : "Save profile"}</button>
              <button className="btn" disabled={busy !== null} onClick={() => setDraft(null)}>Close editor</button>
            </div>
          </div>
        </section>
      )}

      {state.profiles.length === 0 ? (
        <section className="card"><div className="empty">No routing profiles yet. Create one to turn a complete routing policy into a custom relaypool command.</div></section>
      ) : state.profiles.map((profile) => (
        <section className="card" key={profile.id}>
          <div className="card-head">
            <div>
              <p className="card-title" style={{ fontSize: 13 }}>{profile.name}</p>
              <p className="subline"><code>relaypool {profile.command}</code>{profile.aliases.length ? ` · aliases: ${profile.aliases.join(", ")}` : ""}</p>
            </div>
            <div className="row">
              {profile.active && <Badge kind="ok">active</Badge>}
              <Badge kind="neutral">{profile.mode === "pace" ? "weekly pace" : "fixed priority"}</Badge>
            </div>
          </div>
          <div className="card-body">
            <p style={{ marginTop: 0 }}>{profile.sentence}</p>
            <p className="note">Default: {nameOf(profile.default_workspace_id ?? "") || "global default"} · handoff floor: {pct(profile.handoff_below_percent)}</p>
            {profile.subagent_helper_enabled && (
              <p className="note">
                Luna helpers: {nameOf(profile.subagent_helper_workspace_id ?? "")} · {profile.subagent_helper_model}
              </p>
            )}
            {profile.disabled_workspace_ids.length > 0 && <p className="note">Disabled here: {profile.disabled_workspace_ids.map(nameOf).join(", ")}</p>}
            <div className="actions">
              <button
                className="btn primary"
                disabled={profile.active || busy !== null}
                onClick={() => execute(`activate-${profile.id}`, async () => {
                  const result = await api.activateProfile(profile.command);
                  setOK(`Activated ${result.profile.name}. ${result.decision.summary}`);
                })}
              >
                {busy === `activate-${profile.id}` ? "Activating..." : profile.active ? "Active" : "Activate"}
              </button>
              <button className="btn" disabled={busy !== null} onClick={() => setDraft({ ...profile })}><IconPencil className="ico-sm" />Edit</button>
              <button className="btn" disabled={busy !== null} onClick={() => duplicate(profile)}>Duplicate</button>
              <button
                className="btn danger"
                disabled={profile.active || busy !== null}
                title={profile.active ? "Activate another profile before deleting this one." : undefined}
                onClick={() => {
                  if (!window.confirm(`Delete the routing profile “${profile.name}”?`)) return;
                  void execute(`delete-${profile.id}`, async () => {
                    await api.deleteProfile(profile.id);
                    if (draft?.id === profile.id) setDraft(null);
                    setOK(`Deleted ${profile.name}.`);
                  });
                }}
              >
                <IconTrash className="ico-sm" />
                {busy === `delete-${profile.id}` ? "Deleting..." : "Delete"}
              </button>
            </div>
          </div>
        </section>
      ))}
    </>
  );
}
