import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { connect, type Namespace } from "../src/api/client";
import App from "../src/App.svelte";
import { capacity, ceilings, condition, counted, offered, reachedBy, type Pool, type Runner } from "../src/lib/fleet";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario } from "./scenario";

const now = Date.parse("2026-09-30T06:02:30Z");
const dana = scenario("dana");
const runners = (dana["GET /api/v1/runners"]!.body as { runners: Runner[] }).runners;
const pools = (dana["GET /api/v1/runner-pools"]!.body as { runner_pools: { pool: Pool }[] }).runner_pools.map((p) => p.pool);
const namespaces = (dana["GET /api/v1/namespaces"]!.body as { namespaces: Namespace[] }).namespaces;
const named = (id: string) => runners.find((r) => r.runner === id)!;
const pool = (name: string) => pools.find((p) => p.name === name)!;

describe("a runner's condition", () => {
  it("is the installation's order first, then whether it is heard from, then what it says of itself", () => {
    expect(condition(named("runner-dmz-00"), now)).toBe("revoked");
    expect(condition(named("runner-dmz-03"), now)).toBe("draining");
    expect(condition(named("runner-gpu-01"), now)).toBe("unhealthy");
    expect(condition(named("runner-dmz-01"), now)).toBe("ready");
    expect(condition({ ...named("runner-dmz-03"), reported_state: "ready" }, now)).toBe("draining");
  });

  it("is silent past three missed heartbeats, 30 s, and before its first", () => {
    const r = named("runner-dmz-01");
    expect(condition(r, Date.parse(r.last_seen_at!) + 30_000)).toBe("ready");
    expect(condition(r, Date.parse(r.last_seen_at!) + 30_001)).toBe("silent");
    const { last_seen_at: _, reported_state: __, concurrency: ___, ...joined } = r;
    expect(condition(joined, now)).toBe("silent");
  });
});

describe("a pool", () => {
  it("offers the concurrency of its runners ready now, as the pools' statistics count its capacity", () => {
    const of = (p: string) => runners.filter((r) => r.pool === p);
    expect(["default", "dmz", "lab", "gpu"].map((p) => offered(of(p), now))).toEqual([12, 8, 2, 0]);
    expect(counted(of("dmz"), now)).toBe("2 ready · 1 draining · 1 revoked");
  });

  it("is reached by the namespaces it accepts that allow it, by listing it or listing nothing", () => {
    expect(reachedBy(pool("default"), namespaces)).toEqual(["dana", "finance", "team-ops"]);
    expect(reachedBy(pool("dmz"), namespaces)).toEqual(["finance", "team-ops"]);
    expect(reachedBy(pool("gpu"), namespaces)).toEqual(["research"]);
    expect(reachedBy({ ...pool("gpu"), namespaces: ["finance"] }, namespaces)).toEqual([]);
  });

  it("writes its ceilings as a step does, and a host's capacity as measured", () => {
    expect(ceilings(pool("default"))).toBe("none");
    expect(ceilings(pool("gpu"))).toBe("cpu 16 · memory 64Gi · pids 4096");
    expect(capacity(named("runner-default-02"))).toBe("4 vCPU · 16 GiB · 100 GiB disk · arm64");
  });
});

function open(who: string, path: string, search = "") {
  const api = connect("http://stand-in/", answering(scenario(who)));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

describe("the runners and pools", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("list each pool with what it accepts, caps and offers, and who reaches it, to an administrator", async () => {
    open("dana", "/runners");
    const table = within(await screen.findByRole("region", { name: "Pools" }));
    const row = (await table.findByRole("button", { name: "dmz" })).closest("tr")!;
    const cells = [...row.querySelectorAll("td")].map((c) => c.textContent!.trim());
    expect(cells).toEqual(["dmz", "zone=dmzarch=amd64", "finance, team-ops", "cpu 4 · memory 8Gi", "hardened", "2 ready · 1 draining · 1 revoked", "8", "finance, team-ops"]);
    const gpu = [...table.getByRole("button", { name: "gpu" }).closest("tr")!.querySelectorAll("td")].map((c) => c.textContent!.trim());
    expect(gpu.slice(4)).toEqual(["sandboxed", "1 unhealthy", "0", "research"]);
  });

  it("list each runner with its condition, last heartbeat and who took it out of service, never a host", async () => {
    open("dana", "/runners");
    const pane = await screen.findByRole("region", { name: "Runners" });
    const rows = await within(pane).findAllByRole("row");
    expect(rows.slice(1).map((r) => r.querySelector("td")!.textContent)).toEqual([
      "runner-default-01",
      "runner-default-02",
      "runner-dmz-00",
      "runner-dmz-01",
      "runner-dmz-02",
      "runner-dmz-03",
      "runner-gpu-01",
      "runner-lab-01",
    ]);
    const drained = rows.find((r) => r.textContent!.includes("runner-dmz-03"))!;
    expect(within(drained).getByText("draining")).toBeTruthy();
    expect(drained.textContent).toContain("3s ago");
    expect(drained.textContent).toMatch(/drained by dana, .*: kernel update/);
    const revoked = rows.find((r) => r.textContent!.includes("runner-dmz-00"))!;
    expect(revoked.textContent).toMatch(/revoked by dana, .*: disk replaced/);
    expect(revoked.textContent).toContain("its results taken until");
    const behind = within(revoked).getByText("0.5.2");
    expect(behind.getAttribute("title")).toBe("The installation runs 0.6.0");
    expect(within(pane).getByText(/never the host it runs on/)).toBeTruthy();
  });

  it("narrow the runners to the pool chosen, kept in the address", async () => {
    const place = open("dana", "/runners");
    await fireEvent.click(await screen.findByRole("button", { name: "lab" }));
    expect(place.query.get("pool")).toBe("lab");
    const pane = screen.getByRole("region", { name: "Runners" });
    expect(within(pane).getAllByRole("row").slice(1).map((r) => r.querySelector("td")!.textContent)).toEqual(["runner-lab-01"]);
    await fireEvent.click(within(pane).getByRole("button", { name: "Every pool" }));
    expect(place.query.get("pool")).toBeNull();
  });

  it("are reached from the menu, and offered to nobody else", async () => {
    const place = open("dana", "/dana/runs");
    await fireEvent.click(await screen.findByRole("button", { name: /You, dana/ }));
    await fireEvent.click(screen.getByRole("link", { name: "Runners and pools" }));
    expect(place.route).toEqual({ kind: "runners" });
    expect(await screen.findByRole("region", { name: "Pools" })).toBeTruthy();
    cleanup();

    open("alice", "/runners");
    expect(await screen.findByText("No such thing, or not yours.")).toBeTruthy();
  });
});
