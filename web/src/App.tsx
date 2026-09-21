// App shell: tabs, live state, and the connect flow that several screens share.

import { useCallback, useEffect, useRef, useState } from "react";
import type { ActivityRow, LiveStatus, ModelReportRow, Rule, State } from "./api";
import { ApiError, api, boot, subscribeState } from "./api";
import { Activity } from "./Activity";
import { Overview } from "./Overview";
import { Rules, blankRule } from "./Rules";
import { Profiles } from "./Profiles";
import { Advanced } from "./Advanced";
import { Settings } from "./Settings";
import type { Theme } from "./Settings";
import { Workspaces } from "./Workspaces";
import { ErrorBox } from "./ui";
import {
  IconGauge,
  IconLayers,
  IconList,
  IconSettings,
  IconKey,
  IconShield,
} from "./icons";

type Tab = "overview" | "workspaces" | "profiles" | "rules" | "activity" | "settings" | "advanced";

const TABS: { id: Tab; label: string; icon: () => React.ReactElement }[] = [
  { id: "overview", label: "Overview", icon: () => <IconGauge className="ico-sm" /> },
  { id: "workspaces", label: "Workspaces", icon: () => <IconLayers className="ico-sm" /> },
  { id: "profiles", label: "Profiles", icon: () => <IconList className="ico-sm" /> },
  { id: "rules", label: "Rules", icon: () => <IconShield className="ico-sm" /> },
  { id: "activity", label: "Activity", icon: () => <IconList className="ico-sm" /> },
  { id: "settings", label: "Settings", icon: () => <IconSettings className="ico-sm" /> },
  // Advanced is last and holds everything a single-user install never needs.
  { id: "advanced", label: "Advanced", icon: () => <IconKey className="ico-sm" /> },
];

/** BrandMark is the app's own glyph: three stacked layers, matching the Workspaces icon. */
function BrandMark() {
  return (
    <span className="brand-mark" aria-hidden="true">
      <IconLayers className="ico" />
    </span>
  );
}

function readTheme(): Theme {
  const v = localStorage.getItem("codexrelay.theme");
  return v === "light" || v === "dark" ? v : "system";
}

export function App() {
  const [state, setState] = useState<State | null>(null);
  const [activity, setActivity] = useState<ActivityRow[]>([]);
  const [models, setModels] = useState<ModelReportRow[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [live, setLive] = useState<LiveStatus>("connecting");
  const [tab, setTab] = useState<Tab>("overview");
  const [draft, setDraft] = useState<Rule | null>(null);
  const [now, setNow] = useState(() => new Date());
  const [theme, setThemeState] = useState<Theme>(readTheme);
  const [loadingActivity, setLoadingActivity] = useState(true);

  const [connecting, setConnecting] = useState(false);
  const [connectNote, setConnectNote] = useState<string | null>(null);
  const flowRef = useRef<string | null>(null);

  const setTheme = (t: Theme) => {
    setThemeState(t);
    localStorage.setItem("codexrelay.theme", t);
  };

  useEffect(() => {
    const root = document.documentElement;
    if (theme === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", theme);
  }, [theme]);

  const reload = useCallback(() => {
    api
      .state()
      .then((s) => {
        setState(s);
        setLoadError(null);
      })
      .catch((e: unknown) => setLoadError(e instanceof ApiError ? e.message : String(e)));
    api
      .activity(200)
      .then((r) => setActivity(r.activity))
      .catch(() => {
        // Activity is supplementary. A failure here must not blank the whole page.
      })
      .finally(() => setLoadingActivity(false));
    api
      .models()
      .then((r) => setModels(r.models))
      .catch(() => {
        // Model performance is supplementary. A failure must not blank the dashboard.
      });
  }, []);

  useEffect(() => {
    reload();
    const stop = subscribeState(() => reload(), setLive);
    return stop;
  }, [reload]);

  // Time conditions are re-evaluated as time passes, so the rendered "resets in" values
  // must move too rather than freezing at first paint.
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 30_000);
    return () => clearInterval(t);
  }, []);

  // A slow poll behind the event stream: if the stream is down, the page still recovers.
  useEffect(() => {
    if (live === "live") return;
    const t = setInterval(reload, 15_000);
    return () => clearInterval(t);
  }, [live, reload]);

  const connect = useCallback(async () => {
    setConnecting(true);
    setConnectNote(null);
    try {
      const { auth_url, flow_id } = await api.connect();
      flowRef.current = flow_id;
      setConnectNote("Finish signing in in the window that just opened. This page is waiting.");
      window.open(auth_url, "_blank", "noopener,noreferrer");
      const done = await api.connectComplete(flow_id);
      flowRef.current = null;
      setConnectNote(
        done.workspace_ids.length === 1
          ? "Connected 1 workspace."
          : `Connected ${done.workspace_ids.length} workspaces.`,
      );
      reload();
    } catch (e) {
      flowRef.current = null;
      setConnectNote(e instanceof ApiError ? e.message : String(e));
    } finally {
      setConnecting(false);
    }
  }, [reload]);

  const cancelConnect = useCallback(() => {
    const id = flowRef.current;
    flowRef.current = null;
    setConnecting(false);
    setConnectNote("Sign-in cancelled. Nothing was stored.");
    if (id) void api.connectCancel(id).catch(() => undefined);
  }, []);

  const editRule = useCallback(
    (ruleID: string) => {
      const r = state?.rules.find((x) => x.id === ruleID);
      if (r) setDraft({ ...r });
      setTab("rules");
    },
    [state],
  );

  const createRule = useCallback(
    (kind: "prefer" | "reserve", workspaceID: string) => {
      const alt = state?.workspaces.find((w) => w.id !== workspaceID)?.id ?? "";
      setDraft(blankRule(kind, workspaceID, kind === "reserve" ? alt : ""));
      setTab("rules");
    },
    [state],
  );

  if (!state) {
    return (
      <div className="shell">
        <a className="skip-link" href="#main-content">Skip to main content</a>
        <header className="topbar">
          <div className="topbar-inner">
            <span className="brand">
              <BrandMark />
              <span className="brand-text">
                <span className="brand-name">codex-relay</span>
                <span className="brand-ver">{boot.version}</span>
              </span>
            </span>
          </div>
        </header>
        <main id="main-content" className="page">
          <h1 className="sr-only">codex-relay dashboard</h1>
          {loadError ? (
            <section className="card">
              <div className="card-body">
                <ErrorBox error={loadError} />
                <div className="actions mt">
                  <button className="btn" onClick={reload}>
                    Try again
                  </button>
                </div>
              </div>
            </section>
          ) : (
            <section className="card" aria-busy="true">
              <div className="card-body stack">
                <div className="skeleton" style={{ width: "42%", height: 19 }} />
                <div className="skeleton" style={{ width: "68%" }} />
                <div className="skeleton" style={{ width: "55%" }} />
              </div>
            </section>
          )}
        </main>
      </div>
    );
  }

  const liveText =
    live === "live"
      ? "Connected"
      : live === "connecting"
        ? "Connecting…"
        : "Service unreachable";

  return (
    <div className="shell">
      <a className="skip-link" href="#main-content">Skip to main content</a>
      <header className="topbar">
        <div className="topbar-inner">
          <span className="brand">
            <BrandMark />
            <span className="brand-text">
              <span className="brand-name">codex-relay</span>
              <span className="brand-ver">{state.app_version}</span>
            </span>
          </span>

          <nav className="tabs" aria-label="Sections">
            {TABS.map((t) => (
              <button
                key={t.id}
                onClick={() => setTab(t.id)}
                aria-current={tab === t.id ? "page" : undefined}
                title={t.label}
              >
                {t.icon()}
                <span className="tab-label">{t.label}</span>
              </button>
            ))}
          </nav>

          <span className="conn" title={`codex-relay service on ${state.proxy_addr}`}>
            <span className={`dot${live === "live" ? "" : " off"}`} aria-hidden="true" />
            <span className="conn-text">{liveText}</span>
          </span>
        </div>
      </header>

      <main id="main-content" className="page">
        <h1 className="sr-only">codex-relay {TABS.find((item) => item.id === tab)?.label}</h1>
        {loadError && (
          <section className="card">
            <div className="card-body">
              <ErrorBox error={loadError} />
            </div>
          </section>
        )}

        {tab === "overview" && (
          <Overview
            state={state}
            activity={activity}
            now={now}
            onNavigate={setTab}
            onEditRule={editRule}
            onReconnect={connect}
            reload={reload}
          />
        )}
        {tab === "workspaces" && (
          <Workspaces
            state={state}
            now={now}
            reload={reload}
            onCreateRule={createRule}
            connect={connect}
            connecting={connecting}
            connectNote={connectNote}
            cancelConnect={cancelConnect}
          />
        )}
        {tab === "rules" && <Rules state={state} reload={reload} draft={draft} setDraft={setDraft} />}
        {tab === "profiles" && <Profiles state={state} reload={reload} />}
        {tab === "activity" && (
          <Activity state={state} activity={activity} models={models} loading={loadingActivity} />
        )}
        {tab === "settings" && <Settings state={state} theme={theme} setTheme={setTheme} />}
        {tab === "advanced" && <Advanced state={state} />}
      </main>
    </div>
  );
}
