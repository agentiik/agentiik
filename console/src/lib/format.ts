// How the console writes a length of time and an instant, the same on every screen.

// took is a length of time as a person reads it at a glance: 820ms, 22s, 1m 52s, 1h 04m, 3d 02h.
// The second unit is written on two digits, so that a column of them lines up.
export function took(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) {
    return "";
  }
  if (ms < 1000) {
    return `${Math.round(ms)}ms`;
  }
  const s = Math.floor(ms / 1000);
  if (s < 60) {
    return `${s}s`;
  }
  const two = (n: number) => String(n).padStart(2, "0");
  const m = Math.floor(s / 60);
  if (m < 60) {
    return `${m}m ${two(s % 60)}s`;
  }
  const h = Math.floor(m / 60);
  if (h < 24) {
    return `${h}h ${two(m % 60)}m`;
  }
  return `${Math.floor(h / 24)}d ${two(h % 24)}h`;
}

// between is how long a thing took from its start to its end, or to now while it runs.
export function between(start: string | undefined, end: string | undefined, now: number): number | undefined {
  if (!start) {
    return undefined;
  }
  const from = Date.parse(start);
  const to = end ? Date.parse(end) : now;
  return Number.isNaN(from) || Number.isNaN(to) ? undefined : Math.max(0, to - from);
}

// clock is an instant in the reader's own time: the time of day where it is today, the date before
// it otherwise. The full instant, in UTC, is what the element's title and datetime carry.
export function clock(at: string, now: number): string {
  const when = new Date(at);
  if (Number.isNaN(when.getTime())) {
    return at;
  }
  const two = (n: number) => String(n).padStart(2, "0");
  const time = `${two(when.getHours())}:${two(when.getMinutes())}:${two(when.getSeconds())}`;
  const today = new Date(now);
  if (when.getFullYear() === today.getFullYear() && when.getMonth() === today.getMonth() && when.getDate() === today.getDate()) {
    return time;
  }
  return `${when.getFullYear()}-${two(when.getMonth() + 1)}-${two(when.getDate())} ${time}`;
}
