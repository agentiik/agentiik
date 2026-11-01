import { describe, expect, it } from "vitest";
import type { RunDetail } from "../src/lib/run.svelte";
import { mark, ticks, timeline } from "../src/lib/timeline";
import { scenario } from "./scenario";

const failed = scenario("alice")["GET /api/v1/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A"]!.body as RunDetail;

describe("a run's timeline", () => {
  it("draws each step's attempts against the run's span, the one retried paler", () => {
    const drawn = timeline(failed, Date.parse("2026-10-01T00:00:00Z"));
    expect(drawn.length).toBe(112_000);
    expect(drawn.lanes.map((l) => [l.step, l.spans.map((s) => `${s.attempt}:${s.from}-${s.to}:${s.tone}${s.retried ? ":retried" : ""}`)])).toEqual([
      ["normalize", ["1:0-1200:succeeded"]],
      ["invoice", ["1:8000-41000:failed:retried", "2:81000-112000:failed"]],
      ["archive", []],
    ]);
  });

  it("runs to now while the run runs, a shard still going drawn as running", () => {
    const going: RunDetail = { ...failed, state: "running", finished_at: undefined, tasks: failed.tasks.map((t) => (t.attempt === 2 ? { ...t, state: "running", finished_at: undefined } : t)) };
    const drawn = timeline(going, Date.parse("2026-09-30T05:43:03Z"));
    expect(drawn.length).toBe(120_000);
    expect(drawn.lanes[1]!.spans[1]).toEqual({ attempt: 2, from: 81_000, to: 120_000, tone: "running", retried: false });
  });
});

describe("the timeline's marks", () => {
  it("are round from the start, with the end where it stands apart from the last", () => {
    expect(ticks(112_000).map((t) => t.label)).toEqual(["0:00", "0:30", "1:00", "1:30", "1:52"]);
    expect(ticks(61_000).map((t) => t.label)).toEqual(["0:00", "0:30", "1:00"]);
    expect(ticks(3_000).map((t) => t.label)).toEqual(["0:00", "0:01", "0:02", "0:03"]);
  });

  it("read as a clock, past the hour too", () => {
    expect(mark(0)).toBe("0:00");
    expect(mark(65_000)).toBe("1:05");
    expect(mark(3_725_000)).toBe("1:02:05");
  });
});
