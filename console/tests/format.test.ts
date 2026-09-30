import { describe, expect, it } from "vitest";
import { between, took } from "../src/lib/format";

describe("a length of time", () => {
  it("is written in its two largest units, the second on two digits", () => {
    expect(took(820)).toBe("820ms");
    expect(took(22_000)).toBe("22s");
    expect(took(112_000)).toBe("1m 52s");
    expect(took(64 * 60_000)).toBe("1h 04m");
    expect(took(74 * 3_600_000)).toBe("3d 02h");
  });

  it("runs to now while its thing is going, and is nothing before it started", () => {
    expect(between("2026-09-30T06:00:00Z", undefined, Date.parse("2026-09-30T06:00:30Z"))).toBe(30_000);
    expect(between(undefined, undefined, 0)).toBeUndefined();
  });
});
