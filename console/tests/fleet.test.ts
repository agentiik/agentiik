import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { connect, type Namespace } from "../src/api/client";
import App from "../src/App.svelte";
import { capacity, ceilings, condition, counted, joinCommand, joinEnvironment, labelsOf, offered, reachedBy, type Pool, type Runner } from "../src/lib/fleet";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

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
    expect(revoked.textContent).toContain("results accepted until");
    const behind = within(revoked).getByText("0.5.2");
    expect(behind.getAttribute("title")).toBe("The installation runs 0.6.0");
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

  it("are reached from the sidebar, and offered to nobody else", async () => {
    const place = open("dana", "/dana/runs");
    const installation = await screen.findByRole("list", { name: "Installation" });
    await fireEvent.click(within(installation).getByRole("link", { name: "Runners" }));
    expect(place.route).toEqual({ kind: "runners" });
    expect(await screen.findByRole("region", { name: "Pools" })).toBeTruthy();
    cleanup();

    open("alice", "/runners");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});

describe("what joins a host to a pool", () => {
  it("is the command run as root, its labels where it claims any and --replace where it joined before", () => {
    expect(joinCommand("https://agentiik.example.com", "agkjoin_x", ["zone=dmz", "arch=amd64"])).toBe("agk-runner join --api https://agentiik.example.com --token agkjoin_x --labels zone=dmz,arch=amd64");
    expect(joinCommand("https://agentiik.example.com", "agkjoin_x", [], true)).toBe("agk-runner join --api https://agentiik.example.com --token agkjoin_x --replace");
  });

  it("is the environment a container starts serve with, which joins again by itself where its labels change", () => {
    expect(joinEnvironment("https://agentiik.example.com", "agkjoin_x", ["gpu=true"])).toBe("AGK_API=https://agentiik.example.com\nAGK_RUNNER_JOIN_TOKEN=agkjoin_x\nAGK_RUNNER_LABELS=gpu=true");
    expect(joinEnvironment("https://agentiik.example.com", "agkjoin_x", [])).not.toContain("AGK_RUNNER_LABELS");
  });

  it("reads labels apart at commas or spaces, each once", () => {
    expect(labelsOf(" zone=dmz,arch=amd64  zone=dmz, ")).toEqual(["zone=dmz", "arch=amd64"]);
  });
});

// An installation answering from a scenario, keeping each request with its body.
function installation(s: Scenario) {
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, body: text ? JSON.parse(text) : undefined });
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
  return { asked, fetcher };
}

function manage(s: Scenario) {
  const { asked, fetcher } = installation(s);
  const api = connect("http://stand-in/", fetcher);
  const place = new Place({ pathname: "/runners", search: "", baseURI: "https://agentiik.example.com/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return asked;
}

const sent = (asked: { key: string; body: unknown }[], key: string) => asked.filter((a) => a.key === key).map((a) => a.body);

const token = (pool: string, labels: string[]) => ({
  status: 201,
  body: { pool: pools.find((p) => p.name === pool)!, join_token: { id: "01M2AAZ9G62NQXFAFCXKRPJEH5", token: "agkjoin_N8yQ2mVr4K7dLpX0sZaHg5Tf1WbCuJeR9iOnM3vY6kQ", pool, labels, single_use: true, issued_at: "2026-09-30T06:02:30Z", expires_at: "2026-09-30T07:02:30Z" } },
});

describe("managing runners", () => {
  it("creates a pool with its labels, the namespaces it accepts and its ceilings, hardened", async () => {
    const s = scenario("dana");
    s["POST /api/v1/runner-pools"] = { status: 201, body: { pool: { name: "edge", labels: ["zone=edge"], namespaces: ["finance"], resource_ceilings: { cpu: "2", memory: "2Gi" }, containment: "hardened" } } };
    const asked = manage(s);
    await fireEvent.click(await screen.findByRole("button", { name: "New pool" }));
    const form = within(screen.getByRole("form", { name: "New pool" }));
    await fireEvent.input(form.getByLabelText("Name"), { target: { value: "edge" } });
    await fireEvent.input(form.getByLabelText("Labels"), { target: { value: "zone=edge, arch" } });
    expect(form.getByText("arch")).toBeTruthy();
    await fireEvent.input(form.getByLabelText("Labels"), { target: { value: "zone=edge" } });
    await fireEvent.click(form.getByLabelText("finance"));
    await fireEvent.input(form.getByLabelText("CPU"), { target: { value: "2" } });
    await fireEvent.input(form.getByLabelText("Memory"), { target: { value: "2Gi" } });
    await fireEvent.click(form.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("Pool edge created.")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/runner-pools")).toEqual([{ pool: { name: "edge", labels: ["zone=edge"], namespaces: ["finance"], resource_ceilings: { cpu: "2", memory: "2Gi" }, containment: "hardened" } }]);
  });

  it("says a pool's name taken as the API's 409 says it", async () => {
    const s = scenario("dana");
    s["POST /api/v1/runner-pools"] = { status: 409, body: { error: "a runner pool named dmz exists already" } };
    manage(s);
    await fireEvent.click(await screen.findByRole("button", { name: "New pool" }));
    const form = within(screen.getByRole("form", { name: "New pool" }));
    await fireEvent.input(form.getByLabelText("Name"), { target: { value: "dmz" } });
    await fireEvent.click(form.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("A runner pool named dmz exists already.")).toBeTruthy();
  });

  it("adds a runner with a join token for the labels ticked, shown once with the command and the environment that redeem it", async () => {
    const s = scenario("dana");
    s["POST /api/v1/runner-pools/dmz/join-tokens"] = token("dmz", ["zone=dmz"]);
    const asked = manage(s);
    await screen.findByRole("region", { name: "Pools" });
    await fireEvent.click(await screen.findByRole("button", { name: "Add a runner" }));
    const form = within(screen.getByRole("form", { name: "Add a runner" }));
    await fireEvent.change(form.getByLabelText("Pool"), { target: { value: "dmz" } });
    expect((form.getByLabelText("zone=dmz") as HTMLInputElement).checked).toBe(true);
    await fireEvent.click(form.getByLabelText("arch=amd64"));
    await fireEvent.click(form.getByRole("button", { name: "Issue a join token" }));
    expect(await screen.findByText("agk-runner join --api https://agentiik.example.com --token agkjoin_N8yQ2mVr4K7dLpX0sZaHg5Tf1WbCuJeR9iOnM3vY6kQ --labels zone=dmz")).toBeTruthy();
    expect(screen.getByText(/AGK_RUNNER_LABELS=zone=dmz/)).toBeTruthy();
    expect(sent(asked, "POST /api/v1/runner-pools/dmz/join-tokens")).toEqual([{ labels: ["zone=dmz"] }]);
  });

  it("replaces a runner from its row, in its pool with its labels, joining with --replace", async () => {
    const s = scenario("dana");
    s["POST /api/v1/runner-pools/dmz/join-tokens"] = token("dmz", ["zone=dmz", "arch=amd64"]);
    manage(s);
    await fireEvent.click(await screen.findByRole("button", { name: "Orders to runner-dmz-01" }));
    await fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    const dialog = within(screen.getByRole("dialog", { name: "Replace a runner" }));
    expect((dialog.getByLabelText("Pool") as HTMLSelectElement).value).toBe("dmz");
    expect((dialog.getByLabelText("Pool") as HTMLSelectElement).disabled).toBe(true);
    await fireEvent.click(dialog.getByRole("button", { name: "Issue a join token" }));
    expect(await dialog.findByText(/--labels zone=dmz,arch=amd64 --replace$/)).toBeTruthy();
  });

  it("drains and revokes a runner with a reason, and offers a revoked one nothing", async () => {
    const s = scenario("dana");
    const dmz01 = named("runner-dmz-01");
    s["POST /api/v1/runners/runner-dmz-01/drain"] = { status: 200, body: { ...dmz01, state: "draining", drained_by: "dana", drained_at: "2026-09-30T06:02:30Z", drain_reason: "kernel update" } };
    s["POST /api/v1/runners/runner-dmz-02/revoke"] = { status: 403, body: { error: "only an administrator of the installation orders a runner" } };
    const asked = manage(s);
    await fireEvent.click(await screen.findByRole("button", { name: "Orders to runner-dmz-01" }));
    await fireEvent.click(screen.getByRole("button", { name: "Drain" }));
    let form = within(screen.getByRole("form", { name: "Drain a runner" }));
    await fireEvent.input(form.getByLabelText("Reason"), { target: { value: "kernel update" } });
    await fireEvent.click(form.getByRole("button", { name: "Drain" }));
    expect(await screen.findByText("Runner runner-dmz-01 drained.")).toBeTruthy();
    expect(sent(asked, "POST /api/v1/runners/runner-dmz-01/drain")).toEqual([{ reason: "kernel update" }]);

    await fireEvent.click(screen.getByRole("button", { name: "Orders to runner-dmz-02" }));
    await fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    form = within(screen.getByRole("form", { name: "Revoke a runner" }));
    await fireEvent.input(form.getByLabelText("Reason"), { target: { value: "disk replaced" } });
    await fireEvent.click(form.getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText("You do not have permission.")).toBeTruthy();

    expect(screen.queryByRole("button", { name: "Orders to runner-dmz-00" })).toBeNull();
    const drainingOrders = screen.getByRole("button", { name: "Orders to runner-dmz-03" });
    await fireEvent.click(drainingOrders);
    expect(screen.queryByRole("button", { name: "Drain" })).toBeNull();
  });
});
