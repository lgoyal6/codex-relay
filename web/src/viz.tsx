// Small, honest data displays: a donut, a sparkline, a meter.
//
// Every one of these renders a number the SERVICE computed. Nothing here derives a quota
// figure, projects a future one, or converts usage into money. codex-lb's dashboard shows an
// estimated API cost and a burn projection; this build deliberately shows neither, because
// the contract forbids implying that an API-equivalent estimate is real subscription spend,
// and defers prediction-driven scheduling. What is drawn is what was measured.

type Seg = { id: string; label: string; value: number; color: string };

/**
 * Donut renders proportions of a real total. The centre shows the REMAINING amount, because
 * that is the number a person acts on.
 */
export function Donut({
  segments,
  total,
  centerValue,
  centerLabel,
  size = 148,
}: {
  segments: Seg[];
  total: number;
  centerValue: string;
  centerLabel: string;
  size?: number;
}) {
  const stroke = 16;
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  const safeTotal = total > 0 ? total : 1;

  let offset = 0;
  const arcs = segments.map((s) => {
    const frac = Math.max(0, Math.min(1, s.value / safeTotal));
    const arc = { ...s, dash: frac * c, gap: c - frac * c, rot: (offset / safeTotal) * 360 };
    offset += s.value;
    return arc;
  });

  return (
    <div className="donut" style={{ width: size, height: size }}>
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} aria-hidden="true">
        <circle
          cx={size / 2} cy={size / 2} r={r}
          fill="none" stroke="var(--track)" strokeWidth={stroke}
        />
        {arcs.map((a) => (
          <circle
            key={a.id}
            cx={size / 2} cy={size / 2} r={r}
            fill="none" stroke={a.color} strokeWidth={stroke}
            strokeDasharray={`${a.dash} ${a.gap}`}
            transform={`rotate(${-90 + a.rot} ${size / 2} ${size / 2})`}
            strokeLinecap="butt"
          />
        ))}
      </svg>
      <div className="donut-center">
        <span className="donut-label">{centerLabel}</span>
        <span className="donut-value">{centerValue}</span>
      </div>
    </div>
  );
}

/** Legend lists the donut's segments with their real values. */
export function Legend({ segments, suffix = "" }: { segments: Seg[]; suffix?: string }) {
  return (
    <ul className="legend">
      {segments.map((s) => (
        <li key={s.id}>
          <span className="legend-dot" style={{ background: s.color }} aria-hidden="true" />
          <span className="legend-label">{s.label}</span>
          <span className="legend-value">
            {s.value.toLocaleString(undefined, { maximumFractionDigits: 1 })}
            {suffix}
          </span>
        </li>
      ))}
    </ul>
  );
}

/**
 * Sparkline draws observed history. With fewer than two points it draws nothing and says so,
 * rather than inventing a shape from a single reading.
 */
export function Sparkline({ points, color }: { points: number[]; color: string }) {
  if (points.length < 2) {
    return <div className="spark-empty">not enough history yet</div>;
  }
  const w = 120;
  const h = 30;
  const min = Math.min(...points);
  const max = Math.max(...points);
  const span = max - min || 1;
  const step = w / (points.length - 1);
  const d = points
    .map((p, i) => `${i === 0 ? "M" : "L"}${(i * step).toFixed(1)},${(h - ((p - min) / span) * h).toFixed(1)}`)
    .join(" ");
  return (
    <svg className="spark" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden="true">
      <path d={d} fill="none" stroke={color} strokeWidth="1.8" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}

/** Meter is a labelled horizontal bar with an optional threshold marker. */
export function Meter({
  percent,
  markerPercent,
  tone,
}: {
  percent: number;
  markerPercent?: number;
  tone: "ok" | "low" | "crit";
}) {
  const p = Math.max(0, Math.min(100, percent));
  return (
    <div className="meter">
      <div className={`meter-fill ${tone}`} style={{ width: `${p}%` }} />
      {markerPercent !== undefined && (
        <div
          className="meter-marker"
          style={{ left: `${Math.max(0, Math.min(100, markerPercent))}%` }}
          title={`Reserve threshold at ${markerPercent}% remaining`}
        />
      )}
    </div>
  );
}

export const SERIES_COLORS = [
  "var(--series-1)",
  "var(--series-2)",
  "var(--series-3)",
  "var(--series-4)",
  "var(--series-5)",
];
