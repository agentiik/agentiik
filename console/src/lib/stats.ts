// The range every chart of a statistics page is drawn over, as the address holds it, and what the
// API's series become once they are drawn.
//
// "The last hour, 24 hours, 7, 30 or 90 days, or a span chosen, for every chart of the page at
// once." The bucket is left to the API, which picks it from the range so that a chart holds a few
// hundred points at most: a minute up to two hours, 15 minutes up to two days, an hour up to 14 days,
// a day beyond.

export const presets = {
  "1h": { label: "1 hour", ms: 3_600_000 },
  "24h": { label: "24 hours", ms: 86_400_000 },
  "7d": { label: "7 days", ms: 7 * 86_400_000 },
  "30d": { label: "30 days", ms: 30 * 86_400_000 },
  "90d": { label: "90 days", ms: 90 * 86_400_000 },
} as const;

export type Preset = keyof typeof presets;

export type Range = { from: Date; to: Date; preset?: Preset; compare: boolean };

// rangeOf reads the range out of the address: a preset, counted back from now, or a span written out
// with from and to, which a zoom writes; the last 24 hours where it names neither.
export function rangeOf(query: URLSearchParams, now: number): Range {
  const compare = query.get("compare") === "previous";
  const from = Date.parse(query.get("from") ?? "");
  const to = Date.parse(query.get("to") ?? "");
  if (!Number.isNaN(from) && !Number.isNaN(to) && from < to) {
    return { from: new Date(from), to: new Date(to), compare };
  }
  const named = query.get("range");
  const preset: Preset = named !== null && named in presets ? (named as Preset) : "24h";
  return { from: new Date(now - presets[preset].ms), to: new Date(now), preset, compare };
}

// queryOfRange writes a range back into the address, with whatever else the query held.
export function queryOfRange(range: Range, rest: URLSearchParams): URLSearchParams {
  const q = new URLSearchParams(rest);
  for (const key of ["range", "from", "to", "compare"]) q.delete(key);
  if (range.preset) {
    if (range.preset !== "24h") q.set("range", range.preset);
  } else {
    q.set("from", range.from.toISOString());
    q.set("to", range.to.toISOString());
  }
  if (range.compare) q.set("compare", "previous");
  return q;
}

// described is the range in words, for the line above the charts.
export function described(range: Range, bucket: string | undefined): string {
  const day = (d: Date) => d.toISOString().slice(0, 16).replace("T", " ");
  const every: Record<string, string> = { "1m": "a bucket a minute", "15m": "a bucket every 15 minutes", "1h": "a bucket an hour", "1d": "a bucket a day" };
  return `${day(range.from)} to ${day(range.to)} UTC${bucket ? ` · ${every[bucket] ?? bucket}` : ""}`;
}

// seconds is an instant as uPlot's time axis counts it.
export function seconds(at: string): number {
  return Date.parse(at) / 1000;
}

// bytes is a number of bytes as a person reads it.
export function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = 0;
  while (Math.abs(v) >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${u === 0 ? v : v.toFixed(1)} ${units[u]}`;
}

// ms is a length of time in milliseconds as a chart's axis writes it.
export function ms(n: number): string {
  if (n === 0) return "0";
  if (n < 1000) return `${Math.round(n)}ms`;
  if (n < 60_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}s`;
  if (n < 3_600_000) return `${Math.round(n / 60_000)}m`;
  return `${(n / 3_600_000).toFixed(1)}h`;
}
