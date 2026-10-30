import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { band } from "../src/lib/exit";
import { Place } from "../src/lib/place.svelte";
import { lastAttempt, queued, tasksOf, type RunDetail } from "../src/lib/run.svelte";
import { Session } from "../src/lib/session.svelte";
import { answering, scenario, type Scenario } from "./scenario";

const failed = "01JMZ8V1P9C4XQ7K2N4D6F8H0A";

function detail(s: Scenario): RunDetail {
  return s[`GET /api/v1/runs/${failed}`]!.body as RunDetail;
}

function open(path: string, s: Scenario = scenario("alice")) {
  const api = connect("http://stand-in/", answering(s));
  const place = new Place({ pathname: path, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
}

describe("an exit code", () => {
  it("means what the band the documentation puts it in means", () => {
    expect(band(0).name).toBe("success");
    expect(band(42).name).toBe("application failure");
    expect(band(108).name).toBe("transient failure");
    expect(band(120).name).toBe("invalid input");
    expect(band(121).name).toBe("output contract broken");
    expect(band(123).name).toBe("reserved for the runner");
    expect(band(137).name).toBe("reserved for the runtime");
  });
});

describe("a step's tasks", () => {
  const run = detail(scenario("alice"));

  it("are the last attempt first, its shards in order", () => {
    expect(tasksOf(run, "invoice").map((t) => t.task.split("/").slice(2).join("/"))).toEqual(["2/3/8", "1/1/8", "1/2/8", "1/3/8", "1/4/8", "1/5/8", "1/6/8", "1/7/8", "1/8/8"]);
  });

  it("strip to each shard's latest attempt", () => {
    expect(lastAttempt(run, "invoice").map((t) => `${t.shard?.index}:${t.attempt}:${t.state}`)).toEqual(["1:1:succeeded", "2:1:succeeded", "3:2:failed", "4:1:succeeded", "5:1:succeeded", "6:1:succeeded", "7:1:succeeded", "8:1:succeeded"]);
  });

  it("are queued for a slot while dispatched and taken by no runner", () => {
    const task = { task: "r/invoice/1", step: "invoice", attempt: 1 };
    expect(queued({ ...task, state: "dispatched" })).toBe(true);
    expect(queued({ ...task, state: "dispatched", runner: "runner-dmz-01", taken_at: "2026-10-01T05:42:21Z" })).toBe(false);
    // Bound before v0.7.2 kept the instant: the runner still says it is held.
    expect(queued({ ...task, state: "dispatched", runner: "runner-dmz-01" })).toBe(false);
    expect(queued({ ...task, state: "pending" })).toBe(false);
  });
});

describe("the run inspector", () => {
  it("opens on the step that failed and its last failed task, saying what its exit code means", async () => {
    open(`/finance/runs/${failed}`);
    expect(await screen.findByText("invoice · shard 3/8 · attempt 2")).toBeTruthy();
    expect(screen.getByText("exit 108")).toBeTruthy();
    expect(screen.getByText("transient failure: retried as the step's policy says")).toBeTruthy();
  });

  it("says why a step failed where no task can, refused before any started", async () => {
    const s = scenario("alice");
    const step = detail(s).steps.find((st) => st.step === "invoice")!;
    step.reason = "graph: step invoice: port orders: inputs feeds the port a number: a port carries a batch of items, so the value is an object or a list of objects";
    open(`/finance/runs/${failed}`, s);
    expect(await screen.findByText(step.reason)).toBeTruthy();
  });

  it("says a dispatched task nobody took is queued for a slot, and names the runner holding one", async () => {
    const s = scenario("alice");
    const tasks = detail(s).tasks.filter((t) => t.step === "invoice" && t.attempt === 1);
    for (const t of tasks.slice(3, 5)) {
      t.state = "dispatched";
      delete t.exit_code;
      delete t.started_at;
      delete t.finished_at;
    }
    delete tasks[3]!.runner;
    delete tasks[3]!.taken_at;
    tasks[4]!.runner = "runner-dmz-02";
    tasks[4]!.taken_at = "2026-10-01T05:42:21Z";
    open(`/finance/runs/${failed}`, s);
    expect(await screen.findByText("queued for a slot")).toBeTruthy();
    expect(screen.getAllByText("queued for a slot")).toHaveLength(1);
  });

  it("leaves out what a principal without run:read_data may not see, without a word of it", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
    me.permissions["finance/monthly-invoicing"] = ["workflow:read", "run:read"];
    open(`/finance/runs/${failed}`, s);
    expect(await screen.findByText("invoice · shard 3/8 · attempt 2")).toBeTruthy();
    expect(screen.queryByText(/hidden/i)).toBeNull();
    expect(document.body.textContent).not.toContain("run:read_data");
  });

  it("answers a run read under another namespace's address as one that does not exist", async () => {
    open(`/team-ops/runs/${failed}`);
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });

  it("answers a run the API does not give as one that does not exist", async () => {
    open("/finance/runs/01JMZ8ZZZZZZZZZZZZZZZZZZZZ");
    expect(await screen.findByText("This page does not exist, or is not shared with you.")).toBeTruthy();
  });
});
