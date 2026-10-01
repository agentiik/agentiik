import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { fetchable, retention, type Artifact } from "../src/lib/artifacts";
import { filesOf, tokens } from "../src/lib/envelope";
import { streamsFrom, type Source } from "../src/lib/logs.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario, type Scenario } from "./scenario";

const failed = "01JMZ8V1P9C4XQ7K2N4D6F8H0A";
const chosen = `${failed}/invoice/2/3/8`;

// An installation answering from a scenario, keeping each request with its body.
function installation(s: Scenario) {
  const asked: { key: string; search: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    const text = request.method === "GET" ? "" : await request.text();
    asked.push({ key, search: url.search, body: text ? JSON.parse(text) : undefined });
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), {
      status: recorded.status,
      headers: { "Content-Type": "application/json" },
    });
  };
  return { asked, fetcher };
}

function open(path: string, s: Scenario = scenario("alice")) {
  const { asked, fetcher } = installation(s);
  const api = connect("http://stand-in/", fetcher);
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

// A stream the test writes events into, as an EventSource would hand them.
class Stream implements Source {
  onerror: Source["onerror"] = null;
  onopen: Source["onopen"] = null;
  readyState = 1;
  closed = false;
  readonly listeners = new Map<string, ((e: MessageEvent<string>) => void)[]>();
  constructor(readonly url: string) {}
  addEventListener(type: string, listener: (e: MessageEvent<string>) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close(): void {
    this.closed = true;
    this.readyState = 2;
  }
  send(type: string, data: unknown): void {
    for (const l of this.listeners.get(type) ?? []) l(new MessageEvent(type, { data: JSON.stringify(data) }));
  }
}

function streaming(): Stream[] {
  const opened: Stream[] = [];
  streamsFrom((url) => {
    const s = new Stream(url);
    opened.push(s);
    return s;
  });
  return opened;
}

function withPermissions(held: string[]): Scenario {
  const s = scenario("alice");
  const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
  me.permissions["finance/monthly-invoicing"] = held;
  return s;
}

const envelope = {
  meta: { run_id: failed, step: "invoice", port: "error", attempt: 2, count: 1, produced_at: "2026-09-30T05:42:55Z" },
  items: [
    {
      id: "01JMZ8V1PB2C3D4E5",
      data: { customer_id: "C-1043", error: "VAT number not recognised" },
      files: [{ name: "request.json", uri: `agk://run/${failed}/invoice/error/request.json`, media_type: "application/json", size: 2048, sha256: "c1f4a91dd87852afdeab8ce3212cb8fd034a63f37cc523c8fa6d9ead34c1d02b" }],
    },
  ],
};

describe("the log of the task chosen", () => {
  it("follows the step's stream and shows the chosen task's lines alone, until the log is over", async () => {
    const opened = streaming();
    open(`/finance/runs/${failed}?step=invoice&pane=logs`);
    await waitFor(() => expect(opened).toHaveLength(1));
    const stream = opened[0]!;
    expect(new URL(stream.url).pathname).toBe(`/api/v1/runs/${failed}/steps/invoice/logs`);

    stream.send("dispatch", { task_id: `${failed}.invoice.1.1`, idempotency_key: `${failed}/invoice/1/1/8`, attempt: 1, shard: { index: 1, of: 8 }, requeue: 0 });
    stream.send("line", { task_id: `${failed}.invoice.1.1`, line: 1, at: "2026-09-30T05:41:00Z", text: "shard one, which is not chosen" });
    stream.send("dispatch", { task_id: `${failed}.invoice.2.3`, idempotency_key: chosen, attempt: 2, shard: { index: 3, of: 8 }, requeue: 0 });
    stream.send("line", { task_id: `${failed}.invoice.2.3`, line: 1, at: "2026-09-30T05:42:30Z", text: "posting the invoice with token ****" });
    stream.send("gap", { task_id: `${failed}.invoice.2.3`, first_line: 2, lines: 3, reason: "the chunk holding them is gone from the store" });
    stream.send("dispatch_end", { task_id: `${failed}.invoice.2.3`, lines: 4, truncated: true, final: true });

    expect(await screen.findByText("posting the invoice with token ****")).toBeTruthy();
    expect(screen.queryByText("shard one, which is not chosen")).toBeNull();
    expect(screen.getByText(/Lines 2 to 4 were written and cannot be read back: the chunk holding them is gone from the store/)).toBeTruthy();
    expect(screen.getByText(/The runner cut this log at its caps/)).toBeTruthy();

    stream.send("end", { verdict: "failed" });
    expect(await screen.findByText("The step's log is over: failed.")).toBeTruthy();
    expect(stream.closed).toBe(true);
  });

  it("says why the stream was refused, which an EventSource is not told", async () => {
    const opened = streaming();
    const s = scenario("alice");
    s[`GET /api/v1/runs/${failed}/steps/invoice/logs`] = { status: 410, body: { error: "the step's logs went with the run's retention" } };
    open(`/finance/runs/${failed}?step=invoice&pane=logs`, s);
    await waitFor(() => expect(opened).toHaveLength(1));
    const stream = opened[0]!;
    stream.readyState = 2;
    stream.onerror?.call(stream, new Event("error"));
    expect(await screen.findByText("the step's logs went with the run's retention")).toBeTruthy();
  });
});

describe("an envelope", () => {
  it("is drawn for a principal holding run:read_data, its items as JSON and their files listed", async () => {
    const s = scenario("alice");
    s[`GET /api/v1/runs/${failed}/steps/invoice/outputs/error`] = { status: 200, body: envelope };
    open(`/finance/runs/${failed}?step=invoice&port=error`, s);
    expect(await screen.findByText('"VAT number not recognised"')).toBeTruthy();
    expect(screen.getByText("request.json")).toBeTruthy();
    expect(screen.getByText("application/json")).toBeTruthy();
    expect(screen.getByText("c1f4a91dd878")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Copy/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Download/ })).toBeTruthy();
  });

  it("is asked of the task chosen, by its attempt and shard, on the input side", async () => {
    const s = scenario("alice");
    s[`GET /api/v1/runs/${failed}/steps/invoice/inputs/in`] = { status: 200, body: { ...envelope, meta: { ...envelope.meta, port: "in" } } };
    const { asked } = open(`/finance/runs/${failed}?step=invoice&pane=input`, s);
    expect(await screen.findByText('"VAT number not recognised"')).toBeTruthy();
    expect(asked.find((a) => a.key.endsWith("/inputs/in"))?.search).toBe("?attempt=2&shard=3");
  });

  it("says it was purged where the API answers 410", async () => {
    const s = scenario("alice");
    s[`GET /api/v1/runs/${failed}/steps/invoice/outputs/out`] = { status: 410, body: { error: "the envelope was purged" } };
    open(`/finance/runs/${failed}?step=invoice&port=out`, s);
    expect(await screen.findByText(/This envelope was purged with the run's retention/)).toBeTruthy();
  });

  it("is never asked for by a principal without run:read_data", async () => {
    const { asked } = open(`/finance/runs/${failed}?step=invoice&port=error`, withPermissions(["workflow:read", "run:read"]));
    expect(await screen.findByText(/What the envelopes hold is not shown/)).toBeTruthy();
    expect(asked.some((a) => a.key.includes("/outputs/") || a.key.includes("/inputs/"))).toBe(false);
  });
});

describe("acting on a run", () => {
  it("cancels a run still going, once asked twice", async () => {
    const s = scenario("alice");
    (s[`GET /api/v1/runs/${failed}`]!.body as { state: string }).state = "running";
    s[`POST /api/v1/runs/${failed}/cancel`] = { status: 202, body: { run: failed } };
    const { asked } = open(`/finance/runs/${failed}`, s);
    await fireEvent.click(await screen.findByRole("button", { name: "Cancel run" }));
    expect(asked.some((a) => a.key.endsWith("/cancel"))).toBe(false);
    expect(screen.getByText("Cancel this run? Its tasks in flight are stopped.")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    expect(await screen.findByText(/Cancelling was asked/)).toBeTruthy();
    expect(asked.filter((a) => a.key === `POST /api/v1/runs/${failed}/cancel`)).toHaveLength(1);
  });

  it("replays a run that ended from the step chosen, and opens the run it started", async () => {
    const s = scenario("alice");
    const replay = "01JMZ9A2B3C4D5E6F7G8H9J0K1";
    s[`POST /api/v1/runs/${failed}/replay`] = { status: 202, body: { run: replay, state: "queued", commit: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2c4e6b8d01", replay_of: failed, replay_from: "invoice" } };
    const { asked, place } = open(`/finance/runs/${failed}`, s);
    await fireEvent.click(await screen.findByRole("button", { name: "Replay from invoice" }));
    await waitFor(() => expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "runs", run: replay }));
    expect(asked.find((a) => a.key.endsWith("/replay"))?.body).toEqual({ step: "invoice" });
  });

  it("offers the start alone where the run replays from its start only", async () => {
    const s = scenario("alice");
    (s[`GET /api/v1/runs/${failed}`]!.body as { replay_from_start_only: boolean }).replay_from_start_only = true;
    s[`POST /api/v1/runs/${failed}/replay`] = { status: 202, body: { run: "01JMZ9A2B3C4D5E6F7G8H9J0K1", state: "queued", commit: "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2c4e6b8d01", replay_of: failed } };
    const { asked } = open(`/finance/runs/${failed}`, s);
    await fireEvent.click(await screen.findByRole("button", { name: "Replay from the start" }));
    expect(screen.queryByRole("button", { name: "Replay from invoice" })).toBeNull();
    expect(screen.getByText(/replays from its start only/)).toBeTruthy();
    await waitFor(() => expect(asked.find((a) => a.key.endsWith("/replay"))?.body).toEqual({}));
  });

  it("says why a replay was refused", async () => {
    const s = scenario("alice");
    s[`POST /api/v1/runs/${failed}/replay`] = { status: 409, body: { error: "a step above invoice never ended" } };
    open(`/finance/runs/${failed}`, s);
    await fireEvent.click(await screen.findByRole("button", { name: "Replay from invoice" }));
    expect(await screen.findByText("A step above invoice never ended.")).toBeTruthy();
  });

  it("offers neither to a principal without workflow:run", async () => {
    open(`/finance/runs/${failed}`, withPermissions(["workflow:read", "run:read", "run:read_data"]));
    expect(await screen.findByText("invoice · shard 3/8 · attempt 2")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Replay/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Cancel run/ })).toBeNull();
  });
});

describe("an envelope's JSON", () => {
  it("is cut into the tokens the pane colours, a string that looks like JSON staying one string", () => {
    const t = tokens({ a: '{"b": 1}', n: 2, ok: true, none: null, list: [] });
    expect(t.filter((x) => x.kind === "key").map((x) => x.text)).toEqual(['"a"', '"n"', '"ok"', '"none"', '"list"']);
    expect(t.filter((x) => x.kind === "string").map((x) => x.text)).toEqual(['"{\\"b\\": 1}"']);
    expect(t.filter((x) => x.kind === "number").map((x) => x.text)).toEqual(["2"]);
    expect(t.filter((x) => x.kind === "literal").map((x) => x.text)).toEqual(["true", "null"]);
    expect(t.map((x) => x.text).join("")).toBe(JSON.stringify({ a: '{"b": 1}', n: 2, ok: true, none: null, list: [] }, null, 2));
  });

  it("lists every file the items carry, beside the item", () => {
    expect(filesOf(envelope.items).map((f) => `${f.item}:${f.file.name}`)).toEqual(["01JMZ8V1PB2C3D4E5:request.json"]);
  });
});

describe("the files of a step and the outputs of a run", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("lists the files the step published with where each stands, and fetches a live one after asking the route", async () => {
    const heads: { url: string; init?: RequestInit }[] = [];
    vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
      heads.push({ url, init });
      return new Response(null, { status: 200 });
    });
    const clicked: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push(this.href);
    });
    open(`/finance/runs/${failed}?step=invoice&pane=files`);
    const row = (await screen.findByText("request.json")).closest("tr")!;
    expect(row.textContent).toContain("c1f4a91dd878");
    expect(row.textContent).toMatch(/until .*2026-10-07/);
    await fireEvent.click(screen.getByRole("button", { name: /Download/ }));
    const uri = `agk://run/${failed}/invoice/error/request.json`;
    await waitFor(() => expect(clicked.map((href) => new URL(href).pathname)).toEqual([`/api/v1/artifacts/${encodeURIComponent(uri)}`]));
    expect(heads).toHaveLength(1);
    expect(heads[0]!.init?.method).toBe("HEAD");
    expect(heads[0]!.init?.redirect).toBe("manual");
  });

  it("says a file that went since the run was read is finished, rather than missing", async () => {
    vi.stubGlobal("fetch", async () => new Response(null, { status: 410 }));
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    open(`/finance/runs/${failed}?step=invoice&pane=files`);
    await fireEvent.click(await screen.findByRole("button", { name: /Download/ }));
    expect(await screen.findByText(/This file existed and is finished/)).toBeTruthy();
    expect(clicked).not.toHaveBeenCalled();
  });

  it("keeps a collected file in the list, its collection said, with nothing to fetch", async () => {
    open(`/finance/runs/${failed}?step=normalize&pane=files`);
    const row = (await screen.findByText("orders.csv")).closest("tr")!;
    expect(row.textContent).toMatch(/collected .*, its fetches spent/);
    expect(screen.queryByRole("button", { name: /Download/ })).toBeNull();
  });

  it("lists the files to a principal without run:read_data and offers none of them", async () => {
    open(`/finance/runs/${failed}?step=invoice&pane=files`, withPermissions(["workflow:read", "run:read"]));
    expect(await screen.findByText("request.json")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Download/ })).toBeNull();
    expect(screen.getByText(/The files are listed and not fetched/)).toBeTruthy();
  });

  it("opens a workflow output's envelope through the run's outputs route", async () => {
    const s = scenario("alice");
    const detail = s[`GET /api/v1/runs/${failed}`]!.body as { outputs?: Record<string, unknown> };
    detail.outputs = { normalised: { step: "normalize", port: "ok", count: 211 } };
    s[`GET /api/v1/runs/${failed}/outputs/normalised`] = { status: 200, body: { ...envelope, meta: { ...envelope.meta, step: "normalize", port: "ok" } } };
    const { asked, place } = open(`/finance/runs/${failed}`, s);
    const row = (await screen.findByRole("button", { name: "normalised" })).closest("tr")!;
    expect(row.textContent).toMatch(/normalize\.ok/);
    expect(row.textContent).toMatch(/1 · collected/);
    await fireEvent.click(screen.getByRole("button", { name: "normalised" }));
    expect(await screen.findByRole("region", { name: "The envelope of the output normalised" })).toBeTruthy();
    expect(await screen.findByText('"VAT number not recognised"')).toBeTruthy();
    expect(asked.some((a) => a.key === `GET /api/v1/runs/${failed}/outputs/normalised`)).toBe(true);
    expect(place.query.get("output")).toBe("normalised");
  });

  it("says where a file stands, and what a HEAD answered", async () => {
    const live = { status: "live", expires_at: "2026-10-07T05:42:55Z", fetches_left: 1 } as Artifact;
    expect(retention(live, Date.parse("2026-01-01T00:00:00Z"))).toMatch(/^until .*, 1 fetch left$/);
    expect(await fetchable(async () => Object.defineProperty(new Response(null, { status: 200 }), "type", { value: "opaqueredirect" }), "x")).toBe("");
    expect(await fetchable(async () => new Response(null, { status: 409 }), "x")).toMatch(/being served/);
    expect(await fetchable(async () => new Response(null, { status: 404 }), "x")).toBe("No such file, or not yours.");
  });
});
