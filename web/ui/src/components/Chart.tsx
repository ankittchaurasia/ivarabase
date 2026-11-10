import { useEffect, useMemo, useRef, useState } from "react";
import { cn } from "./ui";

export interface ChartSeries {
  key: string;
  label: string;
  /** palette slot 1-4, or "primary" for a single brand-colored series */
  slot?: 1 | 2 | 3 | 4 | "primary";
  points: [number, number | null][];
}

const color = (s: ChartSeries) => (s.slot === "primary" || !s.slot ? "hsl(var(--primary))" : `var(--series-${s.slot})`);

function niceMax(v: number) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

const timeFmt = (ms: number, spanMs: number) =>
  spanMs > 2 * 86400_000
    ? new Date(ms).toLocaleDateString(undefined, { weekday: "short", hour: "2-digit" })
    : new Date(ms).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });

/**
 * Small multi-series line chart: one y-axis, 2px lines, recessive grid,
 * crosshair + tooltip on hover, legend (with latest values) for 2+ series,
 * gaps where there is no data.
 */
export function LineChart({
  series,
  height = 170,
  unit = "",
  yMax,
  format = (v) => (Number.isInteger(v) || Math.abs(v) >= 100 ? v.toFixed(0) : Math.abs(v) >= 10 ? v.toFixed(1) : v.toFixed(2)),
  empty = "No data yet - samples are taken every minute.",
}: {
  series: ChartSeries[];
  height?: number;
  unit?: string;
  yMax?: number;
  format?: (v: number) => string;
  empty?: string;
}) {
  const wrap = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(600);
  const [hover, setHover] = useState<number | null>(null);
  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(220, e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const xs = series[0]?.points.map((p) => p[0]) ?? [];
  const hasData = series.some((s) => s.points.some((p) => p[1] !== null));
  const max = useMemo(() => {
    const m = Math.max(0, ...series.flatMap((s) => s.points.map((p) => p[1] ?? 0)));
    return yMax ?? niceMax(m * 1.1);
  }, [series, yMax]);

  const padL = 40, padR = 12, padT = 8, padB = 22;
  const iw = w - padL - padR, ih = height - padT - padB;
  const x0 = xs[0] ?? 0, x1 = xs[xs.length - 1] ?? 1;
  const X = (t: number) => padL + (x1 === x0 ? 0 : ((t - x0) / (x1 - x0)) * iw);
  const Y = (v: number) => padT + ih - (Math.min(v, max) / max) * ih;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * max);
  const xTicks = xs.length > 1 ? [0, 0.33, 0.66, 1].map((f) => xs[Math.round(f * (xs.length - 1))]) : [];

  const pathOf = (s: ChartSeries) => {
    let d = "";
    let pen = false;
    for (const [t, v] of s.points) {
      if (v === null) {
        pen = false;
        continue;
      }
      d += `${pen ? "L" : "M"}${X(t).toFixed(1)},${Y(v).toFixed(1)}`;
      pen = true;
    }
    return d;
  };

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    if (!xs.length) return;
    const r = (e.currentTarget as SVGSVGElement).getBoundingClientRect();
    const px = e.clientX - r.left;
    const f = (px - padL) / iw;
    setHover(Math.max(0, Math.min(xs.length - 1, Math.round(f * (xs.length - 1)))));
  };

  const lastVal = (s: ChartSeries) => {
    for (let i = s.points.length - 1; i >= 0; i--) if (s.points[i][1] !== null) return s.points[i][1] as number;
    return null;
  };

  return (
    <div ref={wrap} className="relative w-full">
      {series.length > 1 && (
        <div className="mb-2 flex flex-wrap gap-x-4 gap-y-1 text-[12px]">
          {series.map((s) => {
            const v = lastVal(s);
            return (
              <span key={s.key} className="flex items-center gap-1.5 text-muted-foreground">
                <span className="inline-block h-[3px] w-3 rounded-full" style={{ background: color(s) }} />
                {s.label}
                {v !== null && <span className="font-mono text-foreground">{format(v)}{unit}</span>}
              </span>
            );
          })}
        </div>
      )}
      <svg width={w} height={height} className="block overflow-visible" onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img">
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={padL} x2={w - padR} y1={Y(t)} y2={Y(t)} stroke="hsl(var(--border))" strokeDasharray={i === 0 ? undefined : "2 4"} strokeWidth={1} />
            <text x={padL - 6} y={Y(t)} dy="0.32em" textAnchor="end" className="fill-muted-foreground font-mono text-[10px]">
              {format(t)}
            </text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text key={i} x={X(t)} y={height - 6} textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"} className="fill-muted-foreground text-[10px]">
            {timeFmt(t, x1 - x0)}
          </text>
        ))}
        {hasData &&
          series.map((s) => (
            <g key={s.key}>
              <path d={pathOf(s)} fill="none" stroke={color(s)} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
              {/* a sample with no neighbours has no line segment: show it as a dot */}
              {s.points.map(([t, v], i) =>
                v !== null && (s.points[i - 1]?.[1] ?? null) === null && (s.points[i + 1]?.[1] ?? null) === null ? (
                  <circle key={t} cx={X(t)} cy={Y(v)} r={2.5} fill={color(s)} />
                ) : null,
              )}
            </g>
          ))}
        {hover !== null && hasData && (
          <g>
            <line x1={X(xs[hover])} x2={X(xs[hover])} y1={padT} y2={padT + ih} stroke="hsl(var(--muted-foreground))" strokeOpacity={0.5} strokeWidth={1} />
            {series.map((s) => {
              const v = s.points[hover]?.[1];
              return v === null || v === undefined ? null : (
                <circle key={s.key} cx={X(xs[hover])} cy={Y(v)} r={4} fill={color(s)} stroke="hsl(var(--card))" strokeWidth={2} />
              );
            })}
          </g>
        )}
        <rect x={padL} y={padT} width={iw} height={ih} fill="transparent" />
      </svg>
      {!hasData && <div className="pointer-events-none absolute inset-0 grid place-items-center text-[12.5px] text-muted-foreground">{empty}</div>}
      {hover !== null && hasData && (
        <div
          className={cn("pointer-events-none absolute top-6 z-10 min-w-[140px] rounded-md border bg-card px-3 py-2 text-[12px] shadow-pop")}
          style={X(xs[hover]) > w * 0.6 ? { right: w - X(xs[hover]) + 12 } : { left: X(xs[hover]) + 12 }}
        >
          <div className="mb-1 text-muted-foreground">{new Date(xs[hover]).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}</div>
          {series.map((s) => {
            const v = s.points[hover]?.[1];
            return (
              <div key={s.key} className="flex items-center gap-2">
                <span className="inline-block h-2 w-2 rounded-full" style={{ background: color(s) }} />
                <span className="flex-1 text-muted-foreground">{s.label}</span>
                <span className="font-mono">{v === null || v === undefined ? "–" : format(v) + unit}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
