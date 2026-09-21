// Typed client for the local service API.
//
// The session token is handed to the page once, in a JSON data block injected by the Go
// server, and is sent as a header. It is never written to a cookie or a URL.

export type Outcome = "selected" | "blocked";

export type ReasonCode =
  | "default_workspace"
  | "preferred_by_rule"
  | "protected_by_reserve"
  | "protection_evidence_unknown"
  | "paused"
  | "credential_unavailable"
  | "model_not_eligible"
  | "model_eligibility_unknown"
  | "owner_bound_conversation"
  | "owner_bound_but_blocked"
  | "handed_off"
  | "no_eligible_workspace"
  | "reserve_has_no_alternative"
  | "quota_exhausted"
  | "preferred_by_profile"
  | "disabled_by_profile"
  | "weekly_pace"
  | "pace_overflow"
  | "subagent_helper";

export interface Note {
  code: ReasonCode;
  workspace_id?: string;
  rule_id?: string;
  message: string;
  comparison?: string;
}

export interface Candidate {
  workspace_id: string;
  eligible: boolean;
  rank: number;
  reason: ReasonCode;
  detail?: string;
}

export interface Decision {
  outcome: Outcome;
  workspace_id?: string;
  primary_reason: ReasonCode;
  summary: string;
  notes: Note[];
  candidates: Candidate[];
  state_version: number;
  evaluated_at: string;
  simulated: boolean;
  unknown_evidence: boolean;
}

export type Evidence = "fresh" | "stale" | "missing";

export interface WindowView {
  minutes: number;
  label: string;
  remaining_percent: number;
  resets_at: string | null;
  observed_at: string;
  evidence: Evidence;
  reserve_marker_percent?: number;
}

export interface WorkspaceView {
  id: string;
  name: string;
  account_id: string;
  paused: boolean;
  credential_ok: boolean;
  credential_note?: string;
  protected: boolean;
  protected_by?: string;
  windows: WindowView[];
  models_known: boolean;
}

export type RuleKind = "prefer" | "reserve";
export type Comparison = "at_or_below" | "below";
export type ResetComparison = "at_least" | "at_most";
export type NoAlternative = "stop_and_explain" | "use_protected";

export interface Rule {
  id: string;
  kind: RuleKind;
  enabled: boolean;
  priority: number;
  source_workspace_id: string;
  window: { minutes: number };
  comparison: Comparison;
  remaining_percent: number;
  reset_comparison: ResetComparison;
  reset_hours: number;
  preferred_workspace_id: string;
  no_alternative: NoAlternative;
  created_at?: string;
  updated_at?: string;
}

export interface RuleView extends Rule {
  sentence: string;
}

export type ProfileMode = "priority" | "pace";

export interface RoutingProfile {
  id: string;
  name: string;
  command: string;
  aliases: string[];
  mode: ProfileMode;
  priority_workspace_ids: string[];
  pace_workspace_ids: string[];
  overflow_workspace_id?: string;
  target_remaining_percent: number;
  default_workspace_id?: string;
  handoff_below_percent: number;
  disabled_workspace_ids: string[];
  subagent_helper_enabled: boolean;
  subagent_helper_workspace_id?: string;
  subagent_helper_model?: string;
  created_at?: string;
  updated_at?: string;
}

export interface ProfileView extends RoutingProfile {
  active: boolean;
  sentence: string;
}

export interface PaceStanding {
  workspace_id: string;
  remaining_percent: number;
  expected_percent: number;
  delta_percent: number;
  known: boolean;
  status: string;
}

export interface Problem {
  kind: string;
  workspace_id?: string;
  message: string;
  action_label: string;
  action: string;
}

export interface CredentialHealth {
  ok: boolean;
  kind: string;
  detail: string;
  remedy?: string;
}

export interface State {
  version: number;
  workspaces: WorkspaceView[];
  rules: RuleView[];
  proposed: Decision;
  problems: Problem[];
  proxy_addr: string;
  app_version: string;
  now: string;
  credential_storage: CredentialHealth;
  default_workspace_id: string;
  summary: Summary;
  projections: Projection[];
  pool_totals: PoolTotal[];
  profiles: ProfileView[];
  active_profile_id?: string;
  pace_standings: PaceStanding[];
}

export interface Summary {
  window_hours: number;
  turns: number;
  turns_served: number;
  turns_blocked: number;
  conversations: number;
  workspaces: number;
  rules_active: number;
  /** -1 when nothing was measured in the window, which is different from zero. */
  median_first_token_ms: number;
  error_rate_percent: number;
  top_error_class?: string;
  input_tokens: number;
  cached_tokens: number;
  output_tokens: number;
  total_tokens: number;
  /** How many turns reported tokens. Lower than `turns` means partial coverage. */
  turns_with_usage: number;
  /** API-EQUIVALENT, not subscription spend. */
  estimated_cost: number;
  pricing: Pricing;
  turns_series: number[];
  error_series: number[];
  latency_series: number[];
  token_series: number[];
  cost_series: number[];
}

export interface Pricing {
  input_per_mtok: number;
  cached_per_mtok: number;
  output_per_mtok: number;
  currency: string;
  /** false while defaults are in use. */
  user_set: boolean;
}

export interface Projection {
  workspace_id: string;
  workspace_name: string;
  window_minutes: number;
  window_label: string;
  remaining_percent: number;
  burn_percent_per_hour: number;
  /** -1 when nothing is being consumed. */
  hours_to_empty: number;
  hours_to_reset: number;
  will_run_out_before_reset: boolean;
  confidence: "none" | "low" | "moderate";
  basis: string;
}

export interface PoolTotal {
  window_minutes: number;
  window_label: string;
  workspaces: number;
  average_remaining_percent: number;
  account_equivalents: number;
  plans: string[];
  mixed_plans: boolean;
}

export interface ActivityRow {
  id: number;
  at: string;
  thread_id?: string;
  model?: string;
  request_kind: "parent" | "subagent";
  outcome: string;
  workspace_id?: string;
  workspace_name?: string;
  account_email?: string;
  account_plan?: string;
  reason: ReasonCode;
  summary: string;
  notes: Note[];
  candidates: Candidate[];
  attempt: number;
  status_code?: number;
  /** null when never measured; 0 is a real sub-millisecond measurement. */
  first_token_ms: number | null;
  total_ms: number | null;
  error_class?: string;
  /** null when the completed turn did not report usage; 0 is a reported zero. */
  input_tokens: number | null;
  cached_input_tokens: number | null;
  output_tokens: number | null;
  total_tokens: number | null;
  error_message?: string;
  failure_phase?: string;
  upstream_status?: number;
  transport?: string;
  upstream_ms: number | null;
  output_tokens_per_second: number | null;
  quota_snapshot: QuotaSnapshot | null;
}

export interface QuotaSnapshot {
  captured_at: string;
  workspace_id: string;
  windows: QuotaWindowSnapshot[];
}

export interface QuotaWindowSnapshot {
  minutes: number;
  label: string;
  remaining_percent: number;
  resets_at: string | null;
  observed_at: string;
  evidence: Evidence;
}

export interface ModelReportRow {
  model: string;
  turns: number;
  turns_with_usage: number;
  input_tokens: number;
  cached_input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  median_first_token_ms: number | null;
  output_tokens_per_second: number | null;
}

export interface Scenario {
  workspace_id: string;
  window_minutes: number;
  remaining_percent?: number | null;
  reset_in_hours?: number | null;
}

export interface RemovalEffect {
  workspace_id: string;
  bound_threads: string[];
  credentials_gone: boolean;
  explanation: string;
}

export interface Boot {
  token: string;
  version: string;
}

function readBoot(): Boot {
  const el = document.getElementById("codexrelay-boot");
  if (!el || !el.textContent) return { token: "", version: "" };
  try {
    return JSON.parse(el.textContent) as Boot;
  } catch {
    return { token: "", version: "" };
  }
}

export const boot = readBoot();

/** ApiError carries the server's own wording, which is written to be shown to a person. */
/** ApiKey never carries the secret: that exists only in the response that created it. */
export type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked_at: string | null;
  daily_limit: number | null;
};

export type Automation = {
  id: string;
  name: string;
  kind: string;
  workspace_id: string;
  at_minute: number | null;
  enabled: boolean;
  last_run_at: string | null;
  last_result?: string;
  created_at: string;
};

/** RollupBucket is one UTC hour of usage. These outlive pruned decision rows. */
export type RollupBucket = {
  hour: string;
  workspace_id?: string;
  api_key_id?: string;
  turns: number;
  blocked: number;
  errors: number;
  input_tokens: number;
  cached_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  headers.set("X-Codex-Pool-Token", boot.token);
  if (init?.body) headers.set("Content-Type", "application/json");
  let resp: Response;
  try {
    resp = await fetch(path, { ...init, headers, cache: "no-store" });
  } catch {
    throw new ApiError(0, "The codex-relay service is not responding. It may have stopped.");
  }
  const text = await resp.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = null;
    }
  }
  if (!resp.ok) {
    const msg =
      body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : text || `Request failed with status ${resp.status}`;
    throw new ApiError(resp.status, msg);
  }
  return body as T;
}

async function download(path: string): Promise<Blob> {
  let resp: Response;
  try {
    resp = await fetch(path, {
      headers: { "X-Codex-Pool-Token": boot.token },
      cache: "no-store",
    });
  } catch {
    throw new ApiError(0, "The codex-relay service is not responding. It may have stopped.");
  }
  if (!resp.ok) {
    const text = await resp.text();
    throw new ApiError(resp.status, text || `Request failed with status ${resp.status}`);
  }
  return resp.blob();
}

export const api = {
  state: () => call<State>("/api/state"),
  activity: (limit = 100) => call<{ activity: ActivityRow[] }>(`/api/activity?limit=${limit}`),
  activityCSV: () => download("/api/activity.csv"),
  models: () => call<{ models: ModelReportRow[]; retained_decisions: number }>("/api/models"),
  diagnostics: () => call<Record<string, unknown>>("/api/diagnostics"),

  saveRule: (rule: Rule) =>
    call<{ saved: boolean; id: string; state_version: number; sentence: string }>("/api/rules", {
      method: "POST",
      body: JSON.stringify(rule),
    }),
  deleteRule: (id: string) =>
    call<{ deleted: boolean; state_version: number }>(`/api/rules/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  preview: (rules: Rule[] | null, model: string, scenarios: Scenario[]) =>
    call<{ decision: Decision; simulated: boolean; state_version: number; evaluated_at: string }>(
      "/api/rules/preview",
      { method: "POST", body: JSON.stringify({ rules, model, scenarios }) },
    ),

  saveProfile: (profile: RoutingProfile) =>
    call<{ profile: RoutingProfile }>("/api/profiles", {
      method: "POST",
      body: JSON.stringify(profile),
    }),
  deleteProfile: (id: string) =>
    call<{ deleted: boolean }>(`/api/profiles/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  activateProfile: (command: string) =>
    call<{ profile: RoutingProfile; decision: Decision }>(
      `/api/profiles/${encodeURIComponent(command)}/activate`,
      { method: "POST" },
    ),

  connect: () => call<{ auth_url: string; flow_id: string }>("/api/workspaces/connect", { method: "POST" }),
  connectComplete: (flowID: string) =>
    call<{ workspace_ids: string[] }>("/api/workspaces/connect/complete", {
      method: "POST",
      body: JSON.stringify({ flow_id: flowID }),
    }),
  connectCancel: (flowID: string) =>
    call<{ cancelled: boolean }>("/api/workspaces/connect/cancel", {
      method: "POST",
      body: JSON.stringify({ flow_id: flowID }),
    }),
  pause: (id: string, paused: boolean) =>
    call<{ paused: boolean }>(`/api/workspaces/${encodeURIComponent(id)}/pause`, {
      method: "POST",
      body: JSON.stringify({ paused }),
    }),
  rename: (id: string, name: string) =>
    call<{ renamed: boolean }>(`/api/workspaces/${encodeURIComponent(id)}/rename`, {
      method: "POST",
      body: JSON.stringify({ name }),
    }),
  removalEffect: (id: string) =>
    call<RemovalEffect>(`/api/workspaces/${encodeURIComponent(id)}/removal-effect`),
  remove: (id: string) =>
    call<RemovalEffect>(`/api/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" }),
  refreshWorkspaces: () => call<{ refreshed: boolean }>("/api/workspaces/refresh", { method: "POST" }),
  setPricing: (p: Partial<Pricing>) =>
    call<Pricing>("/api/settings/pricing", { method: "POST", body: JSON.stringify(p) }),
  listKeys: () =>
    call<{ keys: ApiKey[]; require_key: boolean; header: string }>("/api/keys"),
  createKey: (name: string, dailyLimit: number | null) =>
    call<{ key: ApiKey; secret: string }>("/api/keys", {
      method: "POST",
      body: JSON.stringify({ name, daily_limit: dailyLimit }),
    }),
  revokeKey: (id: string) =>
    call<{ revoked: boolean }>(`/api/keys/${encodeURIComponent(id)}`, { method: "DELETE" }),
  setRequireKey: (require: boolean) =>
    call<{ require_key: boolean }>("/api/settings/require-key", {
      method: "POST",
      body: JSON.stringify({ require }),
    }),

  listAutomations: () => call<{ automations: Automation[] }>("/api/automations"),
  createAutomation: (name: string, kind: string, workspaceID: string, atMinute: number | null) =>
    call<{ automation: Automation }>("/api/automations", {
      method: "POST",
      body: JSON.stringify({ name, kind, workspace_id: workspaceID, at_minute: atMinute }),
    }),
  deleteAutomation: (id: string) =>
    call<{ deleted: boolean }>(`/api/automations/${encodeURIComponent(id)}`, { method: "DELETE" }),
  enableAutomation: (id: string, enabled: boolean) =>
    call<{ enabled: boolean }>(`/api/automations/${encodeURIComponent(id)}/enabled`, {
      method: "POST",
      body: JSON.stringify({ enabled }),
    }),

  rollups: (days: number) =>
    call<{ days: number; buckets: RollupBucket[] }>(`/api/rollups?days=${days}`),

  network: () => call<{ upstream_base: string; upstream_proxy: string }>("/api/network"),
  setProxy: (proxy: string) =>
    call<{ upstream_proxy: string }>("/api/network/proxy", {
      method: "POST",
      body: JSON.stringify({ proxy }),
    }),

  setDefaultWorkspace: (id: string) =>
    call<{ default_workspace_id: string }>("/api/settings/default-workspace", {
      method: "POST",
      body: JSON.stringify({ workspace_id: id }),
    }),
};

export type LiveStatus = "connecting" | "live" | "offline";

/**
 * subscribeState follows the service event stream.
 *
 * EventSource cannot send headers, so the stream is read with fetch instead. That keeps the
 * session token in a header and out of cookies and URLs. If the stream drops, the caller is
 * told immediately and this retries with a bounded backoff; routing is unaffected either way,
 * because the dashboard is only a subscriber.
 */
export function subscribeState(
  onVersion: (version: number) => void,
  onStatus: (s: LiveStatus) => void,
): () => void {
  const ctrl = new AbortController();
  let stopped = false;
  let delay = 500;

  const run = async () => {
    while (!stopped) {
      onStatus("connecting");
      try {
        const resp = await fetch("/api/events", {
          headers: { "X-Codex-Pool-Token": boot.token },
          signal: ctrl.signal,
          cache: "no-store",
        });
        if (!resp.ok || !resp.body) throw new Error(`stream returned ${resp.status}`);
        onStatus("live");
        delay = 500;
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          let cut = buf.indexOf("\n\n");
          while (cut >= 0) {
            const frame = buf.slice(0, cut);
            buf = buf.slice(cut + 2);
            for (const line of frame.split("\n")) {
              if (!line.startsWith("data:")) continue;
              try {
                const payload = JSON.parse(line.slice(5).trim()) as { version?: number };
                if (typeof payload.version === "number") onVersion(payload.version);
              } catch {
                // A frame we cannot read is skipped; the next state read corrects us.
              }
            }
            cut = buf.indexOf("\n\n");
          }
        }
      } catch {
        if (stopped) return;
      }
      if (stopped) return;
      onStatus("offline");
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 2, 10_000);
    }
  };
  void run();

  return () => {
    stopped = true;
    ctrl.abort();
  };
}
