// Installation and service settings are deliberately secondary: they are needed once, and
// then rarely. Diagnostics here are the same redacted report the CLI prints.

import { useState } from "react";
import type { State } from "./api";
import { ApiError, api } from "./api";
import { Badge, ErrorBox, OkBox } from "./ui";
import { IconAlert, IconInfo, IconLink, IconSettings } from "./icons";

export type Theme = "system" | "light" | "dark";

export function Settings({
  state,
  theme,
  setTheme,
}: {
  state: State;
  theme: Theme;
  setTheme: (t: Theme) => void;
}) {
  const [diag, setDiag] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = async () => {
    setBusy(true);
    setError(null);
    try {
      setDiag(JSON.stringify(await api.diagnostics(), null, 2));
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const download = () => {
    if (!diag) return;
    const blob = new Blob([diag], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "codexrelay-diagnostics.json";
    a.click();
    URL.revokeObjectURL(url);
  };

  const providerURL = `http://${state.proxy_addr}/backend-api/codex`;

  return (
    <>
      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconLink className="ico-sm" />
            Service
          </h2>
        </div>
        <div className="card-body">
        <table className="grid">
          <tbody>
            <tr>
              <td style={{ width: 200 }}>Listening on</td>
              <td className="mono">{state.proxy_addr}</td>
            </tr>
            <tr>
              <td>Codex points at</td>
              <td className="mono">{providerURL}</td>
            </tr>
            <tr>
              <td>Version</td>
              <td className="mono">{state.app_version}</td>
            </tr>
            <tr>
              <td>State version</td>
              <td className="mono">{state.version}</td>
            </tr>
            <tr>
              <td>Credential storage</td>
              <td>
                <div className="row" style={{ gap: 7 }}>
                  {state.credential_storage.ok ? <Badge kind="ok">working</Badge> : <Badge kind="danger">unavailable</Badge>}
                  <span>{state.credential_storage.kind}</span>
                </div>
                <div className="note">{state.credential_storage.detail}</div>
                {state.credential_storage.remedy && <div className="note">{state.credential_storage.remedy}</div>}
              </td>
            </tr>
          </tbody>
        </table>
        <p className="note" style={{ marginTop: 12 }}>
          codex-relay binds to loopback only. Run <span className="mono">codexrelay setup</span> to write the
          Codex provider entry for you, and <span className="mono">codexrelay rollback</span> to undo it. Rollback
          restores only the lines we added and refuses to overwrite later edits it did not make.
        </p>
        </div>
      </section>

      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconSettings className="ico-sm" />
            Appearance
          </h2>
        </div>
        <div className="card-body">
        <div className="field" style={{ maxWidth: 220 }}>
          <label htmlFor="theme">Theme</label>
          <select id="theme" value={theme} onChange={(e) => setTheme(e.target.value as Theme)}>
            <option value="system">Match system</option>
            <option value="light">Light</option>
            <option value="dark">Dark</option>
          </select>
        </div>
        </div>
      </section>

      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconInfo className="ico-sm" />
            API-equivalent rates
          </h2>
        </div>
        <div className="card-body">
          <p className="subline" style={{ marginTop: 0 }}>
            The cost figure on Overview is what your tokens would cost on the pay-as-you-go
            API. It is <strong>not</strong> what your ChatGPT subscription charges, which is a
            flat fee. These rates are editable because published prices change, and a figure
            built on a hardcoded rate presented as fact would be misleading.
          </p>
          <RateEditor state={state} />
        </div>
      </section>

      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconAlert className="ico-sm" />
            Known limitations
          </h2>
        </div>
        <div className="card-body">
        <p className="note" style={{ margin: "0 0 12px" }}>
          These are real first-release limits, listed so you are not surprised by them later.
        </p>
        <table className="grid">
          <tbody>
            <tr>
              <td style={{ width: 200 }}>File attachments</td>
              <td>
                Files attached through an MCP server are uploaded under the ChatGPT account{" "}
                <strong>Codex itself is signed in to</strong>, not under the pooled workspace that
                serves the turn. Codex sends the upload without any conversation identity on the
                request, so codex-relay cannot tell which workspace the file belongs to, and
                guessing would upload to one account and read from another. Inline images passed
                with <code>--image</code> are unaffected: they travel inside the turn and are
                pooled normally.
              </td>
            </tr>
            <tr>
              <td>Usage totals</td>
              <td>
                Quota is reported <strong>per workspace</strong>. There is no combined pool total,
                because the upstream usage endpoint describes exactly one identity and adding
                unlike plans together would produce a number that means nothing.
              </td>
            </tr>
            <tr>
              <td>Existing conversations</td>
              <td>
                A conversation stays on the workspace that started it. Rules apply to{" "}
                <strong>new</strong> conversations. If an existing one is blocked, codex-relay
                explains why rather than moving it, because moving it mid-conversation is not
                known to be safe.
              </td>
            </tr>
            <tr>
              <td>Quota thresholds</td>
              <td>
                A reserve threshold is a <strong>selection trigger, not a spending cap</strong>. A
                turn already running, or usage from another device, can still cross it.
              </td>
            </tr>
          </tbody>
        </table>
        </div>
      </section>

      <section className="card">
        <div className="card-head">
          <h2 className="card-title">
            <IconInfo className="ico-sm" />
            Diagnostics
          </h2>
        </div>
        <div className="card-body">
        <p className="note" style={{ marginTop: 0 }}>
          The report below is redacted by design: no account names, emails, tokens, conversation ids or
          prompt text. Workspace ids are truncated so rows can be correlated without identifying an
          account.
        </p>
        <div className="actions" style={{ marginTop: 8 }}>
          <button className="btn" onClick={load} disabled={busy}>
            {busy ? "Collecting…" : "Collect diagnostics"}
          </button>
          <button className="btn" onClick={download} disabled={!diag}>
            Download JSON
          </button>
        </div>
        <ErrorBox error={error} />
        {diag && (
          <pre className="compare" style={{ marginTop: 12, maxHeight: 340, overflow: "auto" }}>
            {diag}
          </pre>
        )}
        </div>
      </section>
    </>
  );
}

/** RateEditor makes the cost assumption visible and changeable, so it is never a hidden constant. */
function RateEditor({ state }: { state: State }) {
  const p = state.summary.pricing;
  const [input, setInput] = useState(String(p.input_per_mtok));
  const [cached, setCached] = useState(String(p.cached_per_mtok));
  const [output, setOutput] = useState(String(p.output_per_mtok));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);

  const save = async () => {
    setBusy(true);
    setErr(null);
    setNote(null);
    try {
      await api.setPricing({
        input_per_mtok: Number(input),
        cached_per_mtok: Number(cached),
        output_per_mtok: Number(output),
      });
      setNote("Rates saved. The Overview estimate now uses them.");
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <div className="inline-fields" style={{ marginBottom: 12 }}>
        <div className="field" style={{ maxWidth: 150 }}>
          <label htmlFor="r-in">Input $ / M tokens</label>
          <input id="r-in" type="number" min="0" step="0.01" value={input} onChange={(e) => setInput(e.target.value)} />
        </div>
        <div className="field" style={{ maxWidth: 150 }}>
          <label htmlFor="r-cache">Cached input $ / M</label>
          <input id="r-cache" type="number" min="0" step="0.001" value={cached} onChange={(e) => setCached(e.target.value)} />
        </div>
        <div className="field" style={{ maxWidth: 150 }}>
          <label htmlFor="r-out">Output $ / M tokens</label>
          <input id="r-out" type="number" min="0" step="0.01" value={output} onChange={(e) => setOutput(e.target.value)} />
        </div>
        <button className="btn primary" onClick={save} disabled={busy}>
          {busy ? "Saving…" : "Save rates"}
        </button>
      </div>
      <p className="note" style={{ marginTop: 0 }}>
        {p.user_set
          ? "These are your rates."
          : "These are defaults, not a quoted price list. Set them to whatever your provider actually charges."}
      </p>
      <ErrorBox error={err} />
      <OkBox message={note} />
    </>
  );
}
