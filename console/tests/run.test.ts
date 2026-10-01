import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { band } from "../src/lib/exit";
import { Place } from "../src/lib/place.svelte";
import { lastAttempt, tasksOf, type RunDetail } from "../src/lib/run.svelte";
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
});

describe("the run inspector", () => {
  it("opens on the step that failed and its last failed task, saying what its exit code means", async () => {
    open(`/finance/runs/${failed}`);
    expect(await screen.findByText("invoice · shard 3/8 · attempt 2")).toBeTruthy();
    expect(screen.getByText("exit 108")).toBeTruthy();
    expect(screen.getByText("transient failure: retried as the step's policy says")).toBeTruthy();
  });

  it("says what it does not show a principal without run:read_data", async () => {
    const s = scenario("alice");
    const me = s["GET /api/v1/me"]!.body as { permissions: Record<string, string[]> };
    me.permissions["finance/monthly-invoicing"] = ["workflow:read", "run:read"];
    open(`/finance/runs/${failed}`, s);
    expect(await screen.findByText(/Data hidden/)).toBeTruthy();
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
