// What the statistics of the pools and the runners draw, worked out from GET /api/v1/stats/pools
// alone: how much of what a pool offered was in use, and where each runner fell silent.

import type { components } from "../api/schema";

export type PoolsSeries = components["schemas"]["statsPools"];
export type PoolSeries = PoolsSeries["pools"][number];
export type SlotBucket = components["schemas"]["statsSlotBucket"];
export type RunnerSeries = PoolSeries["runners"][number];
export type Silence = RunnerSeries["silences"][number];

// usage is what a pool's buckets come to: the share of the slots it offered that were in use, the
// most in use in each bucket over what was offered at its end, so that a pool held full reads 100%;
// the peak and when it was reached; and, where the pool offers nothing at the range's end, since
// when it has offered nothing, which a pool whose one runner reported unhealthy reads as.
export function usage(buckets: SlotBucket[]): { share?: number; peak: number; peakOf: number; peakAt?: string; idleSince?: string } {
  let used = 0;
  let offered = 0;
  let peak = 0;
  let peakOf = 0;
  let peakAt: string | undefined;
  for (const b of buckets) {
    used += Math.min(b.slots_in_use_max, b.capacity);
    offered += b.capacity;
    if (b.slots_in_use_max > peak) {
      peak = b.slots_in_use_max;
      peakOf = b.capacity;
      peakAt = b.since;
    }
  }
  let idleSince: string | undefined;
  if (buckets.length > 0 && buckets.at(-1)!.capacity === 0) {
    let i = buckets.length - 1;
    while (i > 0 && buckets[i - 1]!.capacity === 0) i--;
    if (i > 0) idleSince = buckets[i]!.since;
  }
  return { share: offered === 0 ? undefined : (used / offered) * 100, peak, peakOf, peakAt, idleSince };
}

// A silence as the timeline lays it out: where it starts and how long it lasts, as shares of the
// range, and whether its runner's tasks were declared lost, which a silence of 30 s or more is
// what the documentation sets apart: "those of 30 s and more, after which a runner's tasks are
// declared lost".
export type Mark = { silence: Silence; left: number; width: number; lost: boolean };

export const declaredLost = 30_000;

export function marks(silences: Silence[], from: string, to: string): Mark[] {
  const start = Date.parse(from);
  const span = Date.parse(to) - start;
  if (!(span > 0)) return [];
  return silences.map((s) => {
    const at = Date.parse(s.at) - start;
    return {
      silence: s,
      left: Math.max(0, Math.min(100, (at / span) * 100)),
      width: Math.max(0, Math.min(100 - (at / span) * 100, (s.length_ms / span) * 100)),
      lost: s.length_ms >= declaredLost,
    };
  });
}

// ticks are a few instants along a range, on whole hours where the range is a day or less and on
// whole days beyond, as a share of the range each, for the timeline's axis.
export function ticks(from: string, to: string, count = 6): { at: number; label: string }[] {
  const start = Date.parse(from);
  const end = Date.parse(to);
  const span = end - start;
  if (!(span > 0)) return [];
  const steps = [3_600_000, 2 * 3_600_000, 4 * 3_600_000, 6 * 3_600_000, 12 * 3_600_000, 86_400_000, 2 * 86_400_000, 7 * 86_400_000, 14 * 86_400_000];
  const step = steps.find((s) => span / s <= count) ?? steps.at(-1)!;
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  const out: { at: number; label: string }[] = [];
  for (let t = Math.ceil(start / step) * step; t < end; t += step) {
    const d = new Date(t);
    const label = step >= 86_400_000 ? `${d.getUTCDate()} ${months[d.getUTCMonth()]}` : `${String(d.getUTCHours()).padStart(2, "0")}:00`;
    out.push({ at: ((t - start) / span) * 100, label });
  }
  return out;
}
