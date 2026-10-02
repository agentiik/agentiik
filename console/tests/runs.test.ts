import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import { filtersOf, queryOf, RunList } from "../src/lib/runs.svelte";
import { answering, scenario } from "./scenario";

describe("the runs view's filters", () => {
  it("are read from the address and written back leaving out the defaults", () => {
    const f = filtersOf(new URLSearchParams("state=failed&workflow=monthly-invoicing&span=7d&other=x"));
    expect(f).toEqual({ state: "failed", workflow: "monthly-invoicing", span: "7d" });
    expect(queryOf(f).toString()).toBe("state=failed&workflow=monthly-invoicing&span=7d");
    expect(queryOf(filtersOf(new URLSearchParams("state=bogus&span=forever"))).toString()).toBe("");
  });
});

describe("a namespace's runs", () => {
  const now = () => Date.parse("2026-09-30T06:02:30Z");

  it("are asked for as the filters narrow them", async () => {
    const asked: string[] = [];
    const list = new RunList(connect("http://stand-in/", answering(scenario("alice"), asked)), "finance", { state: "failed", span: "1h" }, now);
    await list.read();
    expect(asked).toEqual(["GET /api/v1/runs?namespace=finance&state=failed&since=2026-09-30T05%3A02%3A30.000Z&limit=50"]);
    expect(list.runs).toHaveLength(12);
    expect(list.exhausted).toBe(true);
  });

  it("go on from the oldest shown, which until includes, never showing a run twice", async () => {
    const all = scenario("alice")["GET /api/v1/runs"]!.body as { runs: { run: string; created_at: string }[] };
    const first = all.runs.slice(0, 2);
    const next = all.runs.slice(1, 4);
    const asked: string[] = [];
    let page = 0;
    const fetcher: typeof fetch = async (input) => {
      asked.push(new URL((input as Request).url).search);
      const runs = page++ === 0 ? first : next;
      return new Response(JSON.stringify({ runs }), { status: 200, headers: { "Content-Type": "application/json" } });
    };
    const list = new RunList(connect("http://stand-in/", fetcher), "finance", { span: "all" }, now);
    await list.read();
    list.exhausted = false;
    await list.more();
    expect(asked[1]).toContain(`until=${encodeURIComponent(first[1]!.created_at)}`);
    expect(list.runs.map((r) => r.run)).toEqual(all.runs.slice(0, 4).map((r) => r.run));
  });

  it("say why where the API refused them", async () => {
    const list = new RunList(connect("http://stand-in/", answering({ "GET /api/v1/runs": { status: 400, body: { error: "\"x\" is not a run state" } } })), "finance", { span: "all" }, now);
    await list.read();
    expect(list.refused?.why).toBe("\"x\" is not a run state.");
    expect(list.runs).toEqual([]);
  });
});
