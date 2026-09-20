// Small shared pieces. Everything here renders values the service computed; nothing in the
// dashboard derives a quota number of its own.

import type { ReactNode } from "react";
import type { Evidence, WindowView } from "./api";
import {
  IconAlert,
  IconCheck,
  IconChevronRight,
  IconClock,
  IconFlask,
  IconInfo,
  IconPlay,
} from "./icons";

export type BadgeKind = "ok" | "warn" | "danger" | "protect" | "neutral" | "info" | "next";

/**
 * Badge carries state as an icon PLUS a word, never colour alone, so the meaning survives
 * greyscale and colour blindness.
 */
export function Badge({
  kind,
  children,
  title,
  icon = true,
}: {
  kind: BadgeKind;
  children: ReactNode;
  title?: string;
  icon?: boolean;
}) {
  return (
    <span className={`badge ${kind}`} title={title}>
      {icon && badgeIcon(kind)}
      {children}
    </span>
  );
}

function badgeIcon(kind: BadgeKind) {
  switch (kind) {
    case "ok":
      return <IconCheck />;
    case "next":
      return <IconPlay />;
    case "warn":
    case "danger":
      return <IconAlert />;
    case "protect":
      return <IconShieldSmall />;
    case "info":
      return <IconInfo />;
    default:
      return null;
  }
}

function IconShieldSmall() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeLinecap="round"
      strokeLinejoin="round" aria-hidden="true" focusable="false">
      <path d="M12 3 5 6v5.5c0 4.2 3 7.7 7 9.5 4-1.8 7-5.3 7-9.5V6l-7-3Z" />
    </svg>
  );
}

/** SimBadge marks any value that came from a hypothetical scenario rather than live data. */
export function SimBadge() {
  return (
    <span className="badge info" title="This result used values you typed, not live quota.">
      <IconFlask />
      simulated values
    </span>
  );
}

/** Card is the standard panel: an optional titled header, a body, an optional footer. */
export function Card({
  title,
  icon,
  actions,
  children,
  footer,
  tight,
}: {
  title?: ReactNode;
  icon?: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  tight?: boolean;
}) {
  return (
    <section className="card">
      {title && (
        <header className="card-head">
          <h2 className="card-title">
            {icon}
            {title}
          </h2>
          {actions && <div className="actions">{actions}</div>}
        </header>
      )}
      {children && <div className={tight ? "card-body tight" : "card-body"}>{children}</div>}
      {footer && <div className="card-foot">{footer}</div>}
    </section>
  );
}

/** EmptyState explains what is missing and what to do, rather than showing a bare line. */
export function EmptyState({
  icon,
  title,
  children,
  action,
}: {
  icon: ReactNode;
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <div className="empty-ico">{icon}</div>
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action}
    </div>
  );
}

export function ErrorBox({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <div className="err" role="alert">
      <IconAlert />
      <span>{error}</span>
    </div>
  );
}

export function OkBox({ message }: { message: string | null }) {
  if (!message) return null;
  return (
    <div className="ok-box" role="status">
      <IconCheck />
      <span>{message}</span>
    </div>
  );
}

export function Expando({ summary, children }: { summary: string; children: ReactNode }) {
  return (
    <details className="expando">
      <summary>
        <IconChevronRight className="ico-sm" />
        {summary}
      </summary>
      <div className="expando-body">{children}</div>
    </details>
  );
}

export function pct(v: number): string {
  const r = Math.round(v * 10) / 10;
  return `${Number.isInteger(r) ? r.toFixed(0) : r.toFixed(1)}%`;
}

/** untilText renders a reset time as a duration from now, with no calendar rounding. */
export function untilText(iso: string | null, now: Date): string {
  if (!iso) return "no reset time reported";
  const ms = new Date(iso).getTime() - now.getTime();
  if (Number.isNaN(ms)) return "no reset time reported";
  if (ms <= 0) return "resetting now";
  const hours = ms / 3_600_000;
  if (hours < 1) return `in ${Math.max(1, Math.round(ms / 60_000))} min`;
  if (hours < 48) return `in ${hours < 10 ? hours.toFixed(1) : Math.round(hours)} h`;
  return `in ${(hours / 24).toFixed(1)} days`;
}

/** resetAtText pairs a local calendar timestamp with the countdown a person plans around. */
export function resetAtText(iso: string | null, now: Date): string {
  if (!iso) return "Reset time unavailable";
  const reset = new Date(iso);
  if (Number.isNaN(reset.getTime())) return "Reset time unavailable";
  if (reset.getTime() <= now.getTime()) return "Resetting now";

  const absolute = new Intl.DateTimeFormat(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  }).format(reset);
  return `Resets ${absolute} · ${untilText(iso, now)}`;
}

export function agoText(iso: string, now: Date): string {
  const ms = now.getTime() - new Date(iso).getTime();
  if (Number.isNaN(ms)) return "unknown";
  if (ms < 60_000) return "just now";
  const min = ms / 60_000;
  if (min < 60) return `${Math.round(min)} min ago`;
  const h = min / 60;
  if (h < 48) return `${Math.round(h)} h ago`;
  return `${Math.round(h / 24)} days ago`;
}

export function clockText(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

export function evidenceBadge(evidence: Evidence, observedAt: string, now: Date) {
  if (evidence === "missing") {
    return (
      <Badge kind="neutral" title="No reading has been observed for this window.">
        no reading
      </Badge>
    );
  }
  if (evidence === "stale") {
    return (
      <Badge kind="warn" title={`Last observed ${agoText(observedAt, now)}.`}>
        stale reading
      </Badge>
    );
  }
  return null;
}

/**
 * QuotaBar shows remaining percent with the reserve threshold marked on the same scale, so
 * "how close am I to the reserve" is readable without arithmetic.
 */
export function QuotaBar({ w, now }: { w: WindowView; now: Date }) {
  const remaining = Math.max(0, Math.min(100, w.remaining_percent));
  const marker = w.reserve_marker_percent;
  let tone = "";
  if (remaining <= 10) tone = " crit";
  else if (remaining <= 25 || (marker !== undefined && remaining <= marker)) tone = " low";

  return (
    <div className="quota">
      <div className="quota-top">
        <span className="quota-pct">{pct(remaining)}</span>
        <span className="quota-label">{w.label}</span>
      </div>
      <div
        className="bar"
        role="img"
        aria-label={`${w.label}: ${pct(remaining)} remaining${
          marker !== undefined ? `, reserve threshold at ${pct(marker)}` : ""
        }`}
      >
        <div className={`fill${tone}`} style={{ width: `${remaining}%` }} />
        {marker !== undefined && (
          <div
            className="marker"
            style={{ left: `${Math.max(0, Math.min(100, marker))}%` }}
            title={`Reserve threshold: ${pct(marker)} remaining`}
          />
        )}
      </div>
      <div className="quota-foot">
        <IconClock className="ico-sm" />
        <span>{untilText(w.resets_at, now)}</span>
        {evidenceBadge(w.evidence, w.observed_at, now)}
      </div>
    </div>
  );
}

/** reasonText turns a machine reason code into the wording the dashboard shows. */
export function reasonText(code: string): string {
  switch (code) {
    case "default_workspace":
      return "Default choice";
    case "preferred_by_rule":
      return "Preferred by a rule";
    case "protected_by_reserve":
      return "Protected by a reserve rule";
    case "protection_evidence_unknown":
      return "Protected: quota could not be confirmed";
    case "paused":
      return "Paused";
    case "credential_unavailable":
      return "Sign-in needs attention";
    case "model_not_eligible":
      return "Model not available here";
    case "model_eligibility_unknown":
      return "Model availability unknown";
    case "owner_bound_conversation":
      return "Bound to this conversation";
    case "owner_bound_but_blocked":
      return "Bound conversation is blocked";
    case "handed_off":
      return "Moved between accounts";
    case "no_eligible_workspace":
      return "No eligible workspace";
    case "reserve_has_no_alternative":
      return "Reserve rule has no alternative";
    case "quota_exhausted":
      return "Quota exhausted";
    default:
      return code;
  }
}
