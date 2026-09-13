import React from "react";
import { api, ApiError, type ApiKey } from "./api";
import { Card } from "./ui";
import { IconKey, IconShield, IconTrash } from "./icons";

/**
 * Advanced holds the features a single-user install does not need. Everything here is off by
 * default, because the simple path must stay simple: a user who never opens this tab should
 * see no change in behaviour.
 */
export function Advanced() {
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
    </>
  );
}
