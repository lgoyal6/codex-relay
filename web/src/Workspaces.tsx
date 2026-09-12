// Workspaces: connect, name, choose the default identity, pause admission, reconnect, and
// remove. Every control states what it does to conversations that already exist.

import { useState } from "react";
import type { RemovalEffect, State, WorkspaceView } from "./api";
import { ApiError, api } from "./api";
import { Badge, ErrorBox, Expando, QuotaBar } from "./ui";
import {
  IconKey,
  IconLayers,
  IconPause,
  IconPencil,
  IconPin,
  IconPlay,
  IconPlus,
  IconRefresh,
  IconShield,
  IconShieldCheck,
  IconTrash,
} from "./icons";

function message(e: unknown): string {
  return e instanceof ApiError ? e.message : String(e);
}

export function Workspaces({
  state,
  now,
  reload,
  onCreateRule,
  connect,
  connecting,
  connectNote,
  cancelConnect,
}: {
  state: State;
  now: Date;
  reload: () => void;
  onCreateRule: (kind: "prefer" | "reserve", workspaceID: string) => void;
  connect: () => void;
  connecting: boolean;
  connectNote: string | null;
  cancelConnect: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [draftName, setDraftName] = useState("");
  const [removing, setRemoving] = useState<RemovalEffect | null>(null);

  const run = async (key: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    setError(null);
    try {
      await fn();
      reload();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(null);
    }
  };

  const startRename = (w: WorkspaceView) => {
    setRenaming(w.id);
    setDraftName(w.name);
  };

  return (
    <>
      <section className="card">
        <div className="card-head">
          <div>
            <p className="card-title" style={{ fontSize: 13 }}>
              <IconLayers className="ico-sm" />
              Connected workspaces
            </p>
            <p className="subline">
              An account and a ChatGPT workspace are different identities. One account can hold several
              workspaces, so each one is listed and signed in separately.
            </p>
          </div>
          <div className="row">
            <button className="btn" disabled={busy === "refresh"} onClick={() => run("refresh", api.refreshWorkspaces)}>
              <IconRefresh className="ico-sm" />
              {busy === "refresh" ? "Refreshing…" : "Refresh names"}
            </button>
            <button className="btn primary" onClick={connect} disabled={connecting}>
              <IconPlus className="ico-sm" />
              {connecting ? "Waiting for sign-in…" : "Connect a workspace"}
            </button>
          </div>
        </div>
        {connecting && (
          <div className="row" style={{ marginTop: 12 }}>
            <span className="note">
              {connectNote ?? "A browser window was opened for sign-in. This page is waiting for it to finish."}
            </span>
            <button className="btn" onClick={cancelConnect}>
              Cancel
            </button>
          </div>
        )}
        {!connecting && connectNote && (
          <p className="note" style={{ marginTop: 10 }}>
            {connectNote}
          </p>
        )}
        {!state.credential_storage.ok && (
          <p className="err" style={{ marginTop: 12 }} role="alert">
            {state.credential_storage.detail} {state.credential_storage.remedy}
          </p>
        )}
        <ErrorBox error={error} />
      </section>

      {state.workspaces.length === 0 && (
        <section className="card">
          <div className="empty">
            Nothing is connected yet. Connecting opens your browser to sign in to ChatGPT. codex-relay
            keeps its own sign-in for each workspace and never reads or reuses the one Codex holds.
          </div>
        </section>
      )}

      {state.workspaces.map((w) => (
        <section className="card" key={w.id}>
          <div className="card-head">
            <div style={{ minWidth: 0 }}>
              {renaming === w.id ? (
                <form
                  className="inline-fields"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void run(`rename-${w.id}`, async () => {
                      await api.rename(w.id, draftName.trim());
                      setRenaming(null);
                    });
                  }}
                >
                  <div className="field">
                    <label htmlFor={`name-${w.id}`}>Display name</label>
                    <input
                      id={`name-${w.id}`}
                      type="text"
                      value={draftName}
                      autoFocus
                      required
                      onChange={(e) => setDraftName(e.target.value)}
                    />
                  </div>
                  <button className="btn primary" type="submit" disabled={busy === `rename-${w.id}`}>
                    Save
                  </button>
                  <button className="btn" type="button" onClick={() => setRenaming(null)}>
                    Cancel
                  </button>
                </form>
              ) : (
                <>
                  <p style={{ margin: 0, fontSize: 14.5, fontWeight: 600, letterSpacing: "-0.015em" }}>
                    {w.name}
                  </p>
                  <p className="cell-sub mono" style={{ margin: 0 }}>
                    workspace {w.id} · account {w.account_id}
                  </p>
                </>
              )}
            </div>
            <div className="row" style={{ gap: 5 }}>
              {w.id === state.default_workspace_id && (
                <Badge kind="neutral" icon={false}>
                  <IconPin />
                  default
                </Badge>
              )}
              {w.paused ? <Badge kind="warn">paused</Badge> : <Badge kind="ok">admitting</Badge>}
              {!w.credential_ok && <Badge kind="danger">needs sign-in</Badge>}
              {w.protected && <Badge kind="protect">protected now</Badge>}
              {w.models_known ? (
                <Badge kind="neutral">models known</Badge>
              ) : (
                <Badge kind="neutral" title="No model list observed yet, so model eligibility is unknown rather than false.">
                  models unknown
                </Badge>
              )}
            </div>
          </div>

          <div className="card-body">
          {w.credential_note && <p className="note" style={{ marginBottom: 10 }}>{w.credential_note}</p>}

          {w.windows.length > 0 && (
            <div className="row" style={{ gap: 26, marginBottom: 14, alignItems: "flex-start" }}>
              {w.windows.map((win) => (
                <QuotaBar key={win.minutes} w={win} now={now} />
              ))}
            </div>
          )}

          <div className="actions">
            <button className="btn sm" onClick={() => startRename(w)} disabled={renaming === w.id}>
              <IconPencil className="ico-sm" />
              Rename
            </button>
            <button
              className="btn sm"
              disabled={w.id === state.default_workspace_id || busy === `default-${w.id}`}
              onClick={() => run(`default-${w.id}`, () => api.setDefaultWorkspace(w.id))}
            >
              <IconPin className="ico-sm" />
              Use as default
            </button>
            <button className="btn sm" onClick={() => onCreateRule("prefer", w.id)}>
              <IconShield className="ico-sm" />
              Prefer
            </button>
            <button className="btn sm" onClick={() => onCreateRule("reserve", w.id)}>
              <IconShieldCheck className="ico-sm" />
              Protect a reserve
            </button>
            <button
              className="btn sm"
              disabled={busy === `pause-${w.id}`}
              onClick={() => run(`pause-${w.id}`, () => api.pause(w.id, !w.paused))}
            >
              {w.paused ? <IconPlay className="ico-sm" /> : <IconPause className="ico-sm" />}
              {w.paused ? "Resume" : "Pause"}
            </button>
            <button className="btn sm" onClick={connect} disabled={connecting}>
              <IconKey className="ico-sm" />
              Reconnect
            </button>
            <button
              className="btn sm danger"
              disabled={busy === `effect-${w.id}`}
              onClick={() =>
                run(`effect-${w.id}`, async () => {
                  setRemoving(await api.removalEffect(w.id));
                })
              }
            >
              <IconTrash className="ico-sm" />
              Remove…
            </button>
          </div>

          <div style={{ marginTop: 10 }}>
            <Expando summary="What these do">
              <ul style={{ margin: 0, paddingLeft: 18 }} className="note">
                <li>
                  <strong>Pause</strong> stops this workspace taking <em>new</em> conversations. Its stored
                  sign-in is kept and conversations already bound to it keep working.
                </li>
                <li>
                  <strong>Reconnect</strong> signs in again in your browser and replaces the stored
                  credential for this workspace. Existing conversations keep their owner.
                </li>
                <li>
                  <strong>Remove</strong> deletes the stored sign-in from your OS credential store.
                  Conversations bound to this workspace cannot continue afterwards.
                </li>
                <li>
                  <strong>Use as default</strong> chooses this verified workspace identity when no rule
                  orders the choice.
                </li>
              </ul>
            </Expando>
          </div>
          </div>
        </section>
      ))}

      {removing && (
        <section className="card" role="alertdialog" aria-labelledby="remove-title">
          <div className="card-head">
            <h2 id="remove-title" className="card-title">
              <IconTrash className="ico-sm" />
              Remove this workspace?
            </h2>
          </div>
          <div className="card-body">
          <p style={{ marginTop: 0 }}>{removing.explanation}</p>
          {removing.bound_threads.length > 0 && (
            <div className="compare">
              {removing.bound_threads.map((t) => (
                <div key={t}>conversation {t}</div>
              ))}
            </div>
          )}
          <div className="actions">
            <button
              className="btn danger"
              disabled={busy === "remove"}
              onClick={() =>
                run("remove", async () => {
                  const effect = await api.remove(removing.workspace_id);
                  setRemoving(null);
                  setError(null);
                  window.setTimeout(() => alertNote(effect.explanation), 0);
                })
              }
            >
              {busy === "remove" ? "Removing…" : "Remove and delete its sign-in"}
            </button>
            <button className="btn" onClick={() => setRemoving(null)}>
              Keep it
            </button>
          </div>
          </div>
        </section>
      )}
    </>
  );
}

// alertNote reports the outcome of a destructive action. It is deliberately a blocking
// confirmation rather than a toast that can be missed.
function alertNote(text: string) {
  window.alert(text);
}
