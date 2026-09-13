import React from "react";
import { api, ApiError, type ApiKey, type Automation, type RollupBucket } from "./api";
import type { State } from "./api";
import { Card } from "./ui";
import { IconKey, IconShield, IconTrash, IconClock, IconChart, IconGlobe } from "./icons";

/**
 * Advanced holds the features a single-user install does not need. Everything here is off by
 * default, because the simple path must stay simple: a user who never opens this tab should
 * see no change in behaviour.
 */
export function Advanced({ state }: { state: State }) {
  const [keys, setKeys] = React.useState<ApiKey[]>([]);
  const [requireKey, setRequireKey] = React.useState(false);
  const [header, setHeader] = React.useState("X-Codex-Relay-Key");
  const [name, setName] = React.useState("");
  const [limit, setLimit] = React.useState("");
  const [created, setCreated] = React.useState<{ key: ApiKey; secret: string } | null>(null);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      const r = await api.listKeys();
      setKeys(r.keys ?? []);
      setRequireKey(r.require_key);
      setHeader(r.header);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const live = keys.filter((k) => !k.revoked_at);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const parsed = limit.trim() === "" ? null : Number(limit);
      if (parsed !== null && (!Number.isFinite(parsed) || parsed <= 0)) {
        throw new ApiError(400, "A daily limit must be a positive number, or blank for no limit.");
      }
      setCreated(await api.createKey(name.trim(), parsed));
      setName("");
      setLimit("");
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function toggleLock(next: boolean) {
    setError("");
    try {
      await api.setRequireKey(next);
      setRequireKey(next);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <>
      <Card title="API keys" icon={<IconKey className="ico-sm" />}>
        <p className="note" style={{ marginTop: 0 }}>
          A key lets something other than Codex use this pool and get the same quota rules.
          Keys are stored hashed, so the secret is shown once and cannot be recovered.
        </p>

        <form className="key-form" onSubmit={create}>
          <input
            className="input"
            placeholder="What is this key for? e.g. my laptop"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <input
            className="input key-limit"
            placeholder="Daily limit (optional)"
            value={limit}
            inputMode="numeric"
            onChange={(e) => setLimit(e.target.value)}
          />
          <button className="btn primary" disabled={busy || !name.trim()}>
            Create key
          </button>
        </form>

        {error && <div className="callout danger">{error}</div>}

        {created && (
          <div className="callout">
            <div>
              <strong>Copy this now.</strong> It is not shown again and is not recoverable.
              <pre className="secret">{created.secret}</pre>
              <div className="note">
                Send it as the <code>{header}</code> header. For Codex, add to its provider entry:
                <pre className="secret">{`env_http_headers = { "${header}" = "CODEX_RELAY_KEY" }`}</pre>
                then export <code>CODEX_RELAY_KEY</code> in the shell that runs Codex.
              </div>
              <button className="btn sm ghost" onClick={() => setCreated(null)}>
                Done
              </button>
            </div>
          </div>
        )}

        {keys.length === 0 ? (
          <p className="note">No keys yet.</p>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Key</th>
                  <th>Daily limit</th>
                  <th>Last used</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id} className={k.revoked_at ? "row-muted" : undefined}>
                    <td>
                      {k.name}
                      {k.revoked_at && <span className="badge warn" style={{ marginLeft: 6 }}>revoked</span>}
                    </td>
                    <td><code>{k.prefix}…</code></td>
                    <td>{k.daily_limit ?? "none"}</td>
                    <td>{k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "never"}</td>
                    <td className="num">
                      {!k.revoked_at && (
                        <button
                          className="btn sm danger"
                          onClick={async () => {
                            await api.revokeKey(k.id);
                            await load();
                          }}
                        >
                          <IconTrash className="ico-sm" /> Revoke
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card title="Lock the relay" icon={<IconShield className="ico-sm" />}>
        <p className="note" style={{ marginTop: 0 }}>
          The proxy listens on loopback, so by default any program on this machine can send a
          turn through it and spend your quota. Locking it requires a key on every request.
        </p>
        <label className="switch-row">
          <input
            type="checkbox"
            checked={requireKey}
            disabled={live.length === 0 && !requireKey}
            onChange={(e) => void toggleLock(e.target.checked)}
          />
          <span>Require an API key for every request</span>
        </label>
        {live.length === 0 && (
          <p className="note">Create a key first. Locking with no key would make the relay unreachable.</p>
        )}
      </Card>

      <Automations state={state} />
      <LongTermUsage />
      <Network />
    </>
  );
}

const KINDS: { id: string; label: string; needsTime: boolean; help: string }[] = [
  { id: "resume_on_reset", label: "Resume when quota resets", needsTime: false,
    help: "Un-pauses this workspace once its quota window rolls over, so a reset at an awkward hour does not need you watching for it." },
  { id: "pause_daily", label: "Pause at a time each day", needsTime: true,
    help: "Stops new conversations going here from the given time." },
  { id: "resume_daily", label: "Resume at a time each day", needsTime: true,
    help: "Starts using this workspace again from the given time." },
];

/** Automations are scheduled changes to workspace availability, nothing broader. */
function Automations({ state }: { state: State }) {
  const [list, setList] = React.useState<Automation[]>([]);
  const [name, setName] = React.useState("");
  const [kind, setKind] = React.useState(KINDS[0].id);
  const [ws, setWs] = React.useState("");
  const [at, setAt] = React.useState("22:30");
  const [error, setError] = React.useState("");

  const load = React.useCallback(async () => {
    try {
      setList((await api.listAutomations()).automations ?? []);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }, []);
  React.useEffect(() => { void load(); }, [load]);
  React.useEffect(() => {
    if (!ws && state.workspaces.length > 0) setWs(state.workspaces[0].id);
  }, [state.workspaces, ws]);

  const spec = KINDS.find((k) => k.id === kind)!;
  const nameOf = (id: string) => state.workspaces.find((w) => w.id === id)?.name ?? id;

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    try {
      let minute: number | null = null;
      if (spec.needsTime) {
        const [h, m] = at.split(":").map(Number);
        minute = h * 60 + m;
      }
      await api.createAutomation(name.trim(), kind, ws, minute);
      setName("");
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <Card title="Automations" icon={<IconClock className="ico-sm" />}>
      <p className="note" style={{ marginTop: 0 }}>
        Scheduled pauses and resumes. Times are UTC, and a schedule missed while the service
        was stopped does not fire late.
      </p>
      <form className="key-form" onSubmit={create}>
        <input className="input" placeholder="Name" value={name}
          onChange={(e) => setName(e.target.value)} required />
        <select className="input" value={kind} onChange={(e) => setKind(e.target.value)}>
          {KINDS.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
        </select>
        <select className="input" value={ws} onChange={(e) => setWs(e.target.value)}>
          {state.workspaces.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
        </select>
        {spec.needsTime && (
          <input className="input key-limit" type="time" value={at}
            onChange={(e) => setAt(e.target.value)} />
        )}
        <button className="btn primary" disabled={!name.trim() || !ws}>Add</button>
      </form>
      <p className="note">{spec.help}</p>
      {error && <div className="callout danger">{error}</div>}

      {list.length === 0 ? (
        <p className="note">No automations.</p>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr><th>Name</th><th>Does</th><th>Workspace</th><th>Last run</th><th /></tr>
            </thead>
            <tbody>
              {list.map((a) => (
                <tr key={a.id} className={a.enabled ? undefined : "row-muted"}>
                  <td>{a.name}</td>
                  <td>{KINDS.find((k) => k.id === a.kind)?.label ?? a.kind}
                    {a.at_minute != null && ` at ${String(Math.floor(a.at_minute / 60)).padStart(2, "0")}:${String(a.at_minute % 60).padStart(2, "0")} UTC`}
                  </td>
                  <td>{nameOf(a.workspace_id)}</td>
                  <td>{a.last_run_at ? `${new Date(a.last_run_at).toLocaleString()} · ${a.last_result ?? ""}` : "never"}</td>
                  <td className="num">
                    <button className="btn sm ghost" onClick={async () => {
                      await api.enableAutomation(a.id, !a.enabled); await load();
                    }}>{a.enabled ? "Disable" : "Enable"}</button>{" "}
                    <button className="btn sm danger" onClick={async () => {
                      await api.deleteAutomation(a.id); await load();
                    }}><IconTrash className="ico-sm" /></button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

/** LongTermUsage reads the rollups, which are the only record that outlives pruning. */
function LongTermUsage() {
  const [buckets, setBuckets] = React.useState<RollupBucket[]>([]);
  const [days, setDays] = React.useState(30);
  React.useEffect(() => {
    void api.rollups(days).then((r) => setBuckets(r.buckets ?? [])).catch(() => setBuckets([]));
  }, [days]);

  const total = buckets.reduce(
    (acc, b) => ({
      turns: acc.turns + b.turns,
      blocked: acc.blocked + b.blocked,
      errors: acc.errors + b.errors,
      tokens: acc.tokens + b.total_tokens,
    }),
    { turns: 0, blocked: 0, errors: 0, tokens: 0 },
  );

  return (
    <Card title="Long-term usage" icon={<IconChart className="ico-sm" />}>
      <p className="note" style={{ marginTop: 0 }}>
        Activity keeps only the most recent turns and prunes the rest. These hourly totals are
        never pruned, so they are what answers a question about last month.
      </p>
      <div className="key-form">
        {[7, 30, 90].map((d) => (
          <button key={d} className={`btn sm ${d === days ? "primary" : "ghost"}`}
            onClick={() => setDays(d)}>{d} days</button>
        ))}
      </div>
      {buckets.length === 0 ? (
        <p className="note">Nothing recorded yet.</p>
      ) : (
        <div className="tiles">
          <div className="tile"><div className="tile-label">TURNS</div><div className="tile-value">{total.turns.toLocaleString()}</div></div>
          <div className="tile"><div className="tile-label">TOKENS</div><div className="tile-value">{total.tokens.toLocaleString()}</div></div>
          <div className="tile"><div className="tile-label">BLOCKED</div><div className="tile-value">{total.blocked.toLocaleString()}</div></div>
          <div className="tile"><div className="tile-label">ERRORS</div><div className="tile-value">{total.errors.toLocaleString()}</div></div>
        </div>
      )}
    </Card>
  );
}

/** Network configures how the relay reaches upstream. */
function Network() {
  const [proxy, setProxy] = React.useState("");
  const [base, setBase] = React.useState("");
  const [saved, setSaved] = React.useState("");
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    void api.network().then((n) => { setProxy(n.upstream_proxy); setBase(n.upstream_base); }).catch(() => {});
  }, []);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    setError(""); setSaved("");
    try {
      await api.setProxy(proxy.trim());
      setSaved(proxy.trim() ? "Saved. New requests go through this proxy." : "Saved. Connecting directly.");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e));
    }
  }

  return (
    <Card title="Network" icon={<IconGlobe className="ico-sm" />}>
      <p className="note" style={{ marginTop: 0 }}>
        Upstream is <code>{base || "the default backend"}</code>. Set a proxy if this machine
        reaches the internet through one. Leave blank to connect directly, which still honours
        HTTPS_PROXY from the environment.
      </p>
      <form className="key-form" onSubmit={save}>
        <input className="input" placeholder="http://127.0.0.1:8080 (optional)"
          value={proxy} onChange={(e) => setProxy(e.target.value)} />
        <button className="btn primary">Save</button>
      </form>
      {saved && <p className="note">{saved}</p>}
      {error && <div className="callout danger">{error}</div>}
    </Card>
  );
}
