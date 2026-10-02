import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { marks, ticks, usage } from "../src/lib/pool-stats";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

vi.mock("uplot", () => import("./plot"));

const at = (i: number) => new Date(Date.parse("2026-09-30T00:00:00Z") + i * 900_000).toISOString().replace(".000", "");

describe("a pool's slots", () => {
  it("are the share of what was offered that was in use, with the peak and since when nothing is offered", () => {
    const u = usage([
      { since: at(0), until: "", slots_in_use_max: 2, capacity: 4 },
      { since: at(1), until: "", slots_in_use_max: 4, capacity: 4 },
      { since: at(2), until: "", slots_in_use_max: 0, capacity: 0 },
      { since: at(3), until: "", slots_in_use_max: 0, capacity: 0 },
    ]);
    expect(u).toEqual({ share: 75, peak: 4, peakOf: 4, peakAt: at(1), idleSince: at(2) });
    expect(usage([{ since: at(0), until: "", slots_in_use_max: 0, capacity: 0 }]).share).toBeUndefined();
  });
});

describe("a runner's silences", () => {
  it("are laid out as shares of the range, those of 30 s or more set apart as the ones whose tasks were declared lost", () => {
    const m = marks(
      [
        { at: "2026-09-30T06:00:00Z", length_ms: 24_000, tasks_lost: 0 },
        { at: "2026-09-30T12:00:00Z", length_ms: 31_000, tasks_lost: 2 },
      ],
      "2026-09-30T00:00:00Z",
      "2026-10-01T00:00:00Z",
    );
    expect(m.map((x) => [x.left, x.lost])).toEqual([
      [25, false],
      [50, true],
    ]);
    expect(m[1]!.width).toBeCloseTo((31_000 / 86_400_000) * 100);
  });

  it("are read along an axis on whole hours over a day, and whole days beyond", () => {
    expect(ticks("2026-09-30T00:30:00Z", "2026-10-01T00:30:00Z").map((t) => t.label)).toEqual(["04:00", "08:00", "12:00", "16:00", "20:00", "00:00"]);
    const month = ticks("2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z");
    expect(month.length).toBeGreaterThan(2);
    expect(month.every((t) => /^\d{1,2} Sep$/.test(t.label))).toBe(true);
  });
});

function open(who: string, path: string) {
  const api = connect("http://stand-in/", answering(scenario(who)));
  const place = new Place({ pathname: path, search: "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

describe("the pools' statistics", () => {
  it("draw each pool against its capacity, and each runner's heartbeat gaps, to an administrator", async () => {
    open("dana", "/runners/statistics");
    expect(await screen.findByText("Heartbeat gaps")).toBeTruthy();
    expect(screen.getByText(/no capacity since 04:45/)).toBeTruthy();
    const gaps = screen.getByRole("list", { name: /Silences between heartbeats/ });
    expect(within(gaps).getAllByRole("listitem")).toHaveLength(7);
    const lost = within(gaps).getByRole("button", { name: /runner-gpu-01 silent 31s .* 1 task declared lost/ });
    await fireEvent.focus(lost);
    expect(screen.getByText(/runner-gpu-01 silent 31s from 2026-09-30 04:41 UTC, 1 task declared lost/, { selector: ".readout" })).toBeTruthy();
    expect(screen.queryByRole("switch", { name: /Compare/ })).toBeNull();
  });

  it("are reached from the runners and pools, and offered to nobody else", async () => {
    const place = open("dana", "/runners");
    const tabs = await screen.findByRole("navigation", { name: "Runners, what is shown" });
    await fireEvent.click(within(tabs).getByRole("link", { name: "Statistics" }));
    expect(place.route).toEqual({ kind: "runners", tab: "statistics" });
    expect(await screen.findByText("Heartbeat gaps")).toBeTruthy();

    open("alice", "/runners/statistics");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});
