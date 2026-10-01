import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import type { Envelope } from "../src/lib/envelope";
import { Place } from "../src/lib/place.svelte";
import { canonical, candidates, diffSteps, firstDifference, sameItems, summed } from "../src/lib/run-diff";
import type { RunDetail } from "../src/lib/run.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

const failed = "01JMZ8V1P9C4XQ7K2N4D6F8H0A";
const good = "01JMZ8Q6F1T7QK2N4D6F8H0A2F";
const detail = (s: Scenario, run: string) => s[`GET /api/v1/runs/${run}`]!.body as RunDetail;

describe("two runs of one commit", () => {
  it("part at the first step whose verdict, attempts, exit codes, items, image or parameters differ, never at how long it took", () => {
    const s = scenario("alice");
    const steps = diffSteps(detail(s, failed), detail(s, good), true);
    expect(steps.map((x) => [x.step, x.differs])).toEqual([
      ["normalize", []],
      ["invoice", ["verdict", "attempts", "exit codes", "items on error", "items on out"]],
      ["archive", ["verdict", "attempts", "exit codes", "items on ok"]],
    ]);
    expect(firstDifference(steps)).toBe("invoice");
  });

  it("write the exit codes of a step's shards each once, the failures first", () => {
    expect(summed(["0", "0", "108", "0", "1"])).toBe("1 · 108 · 0 ×3");
    expect(summed(["none"])).toBe("none");
  });

  it("are told apart by parameters under run:read_data alone", () => {
    const s = scenario("alice");
    const b = detail(s, good);
    for (const t of b.tasks) if (t.step === "normalize") t.params = { delimiter: ";" } as unknown as typeof t.params;
    expect(diffSteps(detail(s, failed), b, true)[0]!.differs).toEqual(["parameters"]);
    expect(diffSteps(detail(s, failed), b, false)[0]!.differs).toEqual([]);
  });

  it("compare a port's items by their data and files, not by the ids a runner minted", () => {
    const e = (items: { id: string; data: Record<string, unknown> }[]): Envelope => ({ meta: { run_id: "r", step: "s", port: "p", attempt: 1, count: items.length, produced_at: "" }, items: items.map((i) => ({ ...i, files: [] })) });
    const r = sameItems(e([{ id: "a1", data: { n: 1, m: 2 } }, { id: "a2", data: { n: 2 } }, { id: "a3", data: { n: 2 } }]), e([{ id: "b1", data: { m: 2, n: 1 } }, { id: "b2", data: { n: 2 } }, { id: "b3", data: { n: 3 } }]));
    expect(r.same).toBe(2);
    expect(r.onlyFirst.map((i) => i.id)).toEqual(["a3"]);
    expect(r.onlySecond.map((i) => i.id)).toEqual(["b3"]);
    expect(canonical({ b: [1, { d: 1, c: 2 }], a: null })).toBe('{"a":null,"b":[1,{"c":2,"d":1}]}');
  });

  it("offer a failed run a succeeded one of its commit first, and never itself or a run still going", () => {
    const listed = [
      { run: "r1", commit: "c", state: "failed", created_at: "2026-09-30T06:00:00Z" },
      { run: "r2", commit: "c", state: "succeeded", created_at: "2026-09-30T05:00:00Z" },
      { run: "r3", commit: "c", state: "running", created_at: "2026-09-30T07:00:00Z" },
      { run: "r4", commit: "other", state: "succeeded", created_at: "2026-09-30T07:00:00Z" },
      { run: "me", commit: "c", state: "failed", created_at: "2026-09-30T04:00:00Z" },
    ];
    expect(candidates({ run: "me", commit: "c", state: "failed" }, listed).map((r) => r.run)).toEqual(["r2", "r1"]);
    expect(candidates({ run: "me", commit: "c", state: "succeeded" }, listed).map((r) => r.run)).toEqual(["r1", "r2"]);
  });
});

function open(path: string, s: Scenario = scenario("alice"), search = "") {
  const asked: string[] = [];
  const api = connect("http://stand-in/", answering(s, asked));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

describe("the page of two runs", () => {
  it("lays their steps side by side, from the step they part at", async () => {
    open(`/finance/runs/${failed}/against/${good}`);
    expect(await screen.findByText("2 of 3 differ; they part at invoice")).toBeTruthy();
    expect(screen.getByText("first difference")).toBeTruthy();
    expect(screen.getByText("invoice in the two runs")).toBeTruthy();
    expect(screen.getByText("Both were started with the same inputs.")).toBeTruthy();
    expect(screen.getByText("It was dispatched with the same parameters in both.")).toBeTruthy();
  });

  it("compares what a port held when asked, as items and not as envelopes", async () => {
    const { asked } = open(`/finance/runs/${failed}/against/${good}`, scenario("alice"), "?step=normalize");
    const row = (await screen.findAllByRole("row")).find((r) => within(r).queryByText("ok"));
    await fireEvent.click(within(row!).getByRole("button", { name: "Compare the items" }));
    expect(await screen.findByText("212 the same, 2 only in the first, 2 only in the second")).toBeTruthy();
    expect(asked.filter((a) => a.includes("/steps/normalize/outputs/ok"))).toHaveLength(2);
  });

  it("compares no payload without run:read_data, and says so", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
    me.permissions.finance = me.permissions.finance!.filter((p) => p !== "run:read_data");
    open(`/finance/runs/${failed}/against/${good}`, s);
    expect(await screen.findByText(/Their inputs and parameters are not compared/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Compare the items" })).toBeNull();
  });

  it("is reached from the inspector of a run that ended, against a good run of its commit", async () => {
    const { place } = open(`/finance/runs/${failed}`);
    await fireEvent.click(await screen.findByRole("button", { name: "Compare with another run" }));
    const select = (await screen.findByLabelText("Compare with")) as HTMLSelectElement;
    expect(select.value).toBe(good);
    await fireEvent.click(screen.getByRole("link", { name: "Compare" }));
    expect(place.route).toMatchObject({ kind: "namespace", namespace: "finance", view: "runs", run: failed, against: good });
  });

  it("answers as the API does for a run that is not there", async () => {
    open(`/finance/runs/${failed}/against/01JMZ0000000000000000000000`);
    expect(await screen.findByText("No such thing, or not yours.")).toBeTruthy();
  });
});
