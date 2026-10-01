import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { fedBy, inputPorts, layers, layout, scheduling, triggers, what, type Graph } from "../src/lib/graph";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { blocks, stepAt } from "../src/lib/yaml-blocks";
import { answering, scenario } from "./scenario";

const s = scenario("alice");
const detail = s["GET /api/v1/finance/workflows/monthly-invoicing"]!.body as { graph: Graph; version: { commit: string } };
const graph = detail.graph;
const file = s[`GET /api/v1/finance/workflows/monthly-invoicing/tree/${detail.version.commit}?path=agentiik.yaml`]!.text!;

describe("a workflow's graph", () => {
  it("places each step a row below the lowest step it needs, and nothing else", () => {
    expect([...layers(graph)]).toEqual([
      ["normalize", 0],
      ["invoice", 1],
      ["archive", 2],
    ]);
  });

  it("draws an edge per needs entry between the ports it names, and the workflow's inputs and outputs at its boundary", () => {
    const laid = layout(graph);
    expect(laid.edges.map((e) => `${e.from.step}.${e.from.port}>${e.to.step}.${e.to.port}`)).toEqual(["normalize.ok>invoice.in", "invoice.out>archive.invoices", "normalize.rejected>archive.rejected"]);
    expect(laid.edges.map((e) => e.kind)).toEqual(["data", "data", "rejected"]);
    expect(laid.inputs.map((c) => `${c.name}>${c.to?.step}.${c.to?.port}`)).toEqual(["orders>normalize.orders", "customers>normalize.customers"]);
    expect(laid.outputs.map((c) => `${c.from?.step}.${c.from?.port}>${c.name}`)).toEqual(["archive.ok>invoices", "invoice.error>errors"]);
    const archive = laid.nodes.find((n) => n.step === "archive")!;
    expect(archive.inputs.map((p) => p.name)).toEqual(["invoices", "rejected"]);
    expect(archive.y).toBeGreaterThan(laid.nodes.find((n) => n.step === "invoice")!.y);
  });

  it("runs an edge that passes a row beside it, each on a lane of its own, rather than through a step", () => {
    const laid = layout(graph);
    const passing = laid.edges.find((e) => e.from.port === "rejected")!;
    const invoice = laid.nodes.find((n) => n.step === "invoice")!;
    expect(passing.path).toContain(" H ");
    expect(passing.mid.x).toBeGreaterThan(invoice.x + 280);
    const error = laid.outputs.find((c) => c.name === "errors")!;
    const lane = (path: string) => Number(/ V [\d.]+ Q ([\d.]+) /.exec(path.slice(path.indexOf("H")))?.[1]);
    expect(lane(error.path!)).not.toBe(lane(passing.path));
  });

  it("names a step's ports, what it runs, and what decides when it runs, in the file's words", () => {
    expect(inputPorts(graph.steps.normalize!)).toEqual(["orders", "customers"]);
    expect(fedBy("${{ workflow.inputs.orders }}")).toBe("orders");
    expect(fedBy("${{ inputs.orders.items }}")).toBeUndefined();
    expect(scheduling(graph.steps.invoice!)).toEqual(["fan_out: item", "max_parallel: 8", "if: ${{ inputs.in.count > 0 }}", "retry ×3"]);
    expect(scheduling(graph.steps.archive!)).toEqual(["cache"]);
    expect(what(graph.steps.normalize!)).toBe("acme/agk-normalize · normalize-orders 1.4.0");
    const nightly = (s["GET /api/v1/team-ops/workflows/nightly"]!.body as { graph: Graph }).graph;
    expect(what(nightly.steps.export!)).toBe("alpine:3.21 · script, 3 commands");
    expect(what(nightly.steps.reconcile!)).toBe("calls finance/monthly-invoicing@v2.1.0");
    expect(triggers(nightly).event).toEqual([{ type: "run.failed", source: "team-ops/deploy", namespace: "team-ops" }]);
  });
});

describe("a workflow file's blocks", () => {
  it("are the keys under steps, each to its last line, read off the text alone", () => {
    const found = blocks(file);
    expect([...found.keys()]).toEqual(["normalize", "invoice", "archive"]);
    const lines = file.split("\n");
    expect(lines[found.get("invoice")!.start - 1]).toBe("  invoice:");
    expect(lines[found.get("invoice")!.end - 1]).toBe("    outputs: [out, error]");
    expect(stepAt(found, found.get("archive")!.start + 2)).toBe("archive");
    expect(stepAt(found, 1)).toBeUndefined();
    expect([...blocks('steps:\n  ".hidden":\n    image: x\n  a:\n    image: y\nother: 1\n')]).toEqual([
      [".hidden", { start: 2, end: 3 }],
      ["a", { start: 4, end: 5 }],
    ]);
  });
});

function open(path: string, search = "") {
  const api = connect("http://stand-in/", answering(scenario("alice")));
  const place = new Place({ pathname: path, search, baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return place;
}

describe("a workflow's page", () => {
  it("draws the graph with its latest run laid over it, and what starts it", async () => {
    open("/finance/workflows/monthly-invoicing");
    const normalize = await screen.findByRole("button", { name: "Step normalize, brick, succeeded" });
    expect(normalize).toBeTruthy();
    expect(await screen.findByLabelText("3 of 8 shards ended")).toBeTruthy();
    expect(screen.getByText("211 items")).toBeTruthy();
    expect(screen.getByText("0 6 1 * *")).toBeTruthy();
    expect(screen.getByText("POST /hooks/finance/invoicing/rerun")).toBeTruthy();
    expect(screen.getByText("a run waits for the one going")).toBeTruthy();
  });

  it("selects a step from the graph, and its block in the file, and the other way", async () => {
    const place = open("/finance/workflows/monthly-invoicing");
    await fireEvent.click(await screen.findByRole("button", { name: /^Step archive/ }));
    expect(place.query.get("step")).toBe("archive");
    expect(await screen.findByText("cache", { selector: "dd" })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "agentiik.yaml" }));
    const lines = within(await screen.findByRole("list", { name: "agentiik.yaml" })).getAllByRole("listitem");
    const chosen = lines.filter((l) => l.classList.contains("chosen"));
    expect(chosen[0]!.textContent).toContain("archive:");
    expect(chosen.at(-1)!.textContent).toContain("outputs: [ok]");

    const invoiceLine = lines.find((l) => l.textContent?.includes("max_parallel: 8"))!;
    await fireEvent.click(invoiceLine.querySelector(".text")!);
    expect(place.query.get("step")).toBe("invoice");
  });

  it("draws a script by its commands and a call by what it calls", async () => {
    open("/team-ops/workflows/nightly", "?step=export");
    expect(await screen.findByText("agk emit out --from /tmp/rows.json", { exact: false })).toBeTruthy();
    expect(screen.getByText("/bin/sh -e")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: /^Step reconcile/ }));
    expect(await screen.findByText("finance/monthly-invoicing", { selector: "dd" })).toBeTruthy();
    expect(screen.getByText("v2.1.0", { selector: "dd" })).toBeTruthy();
    expect(screen.getByText("no run yet")).toBeTruthy();
  });

  it("lists the history behind its head on asking", async () => {
    open("/finance/workflows/monthly-invoicing");
    await fireEvent.click(await screen.findByRole("button", { name: "History" }));
    expect(await screen.findByText("Archive what normalize rejected beside the invoices")).toBeTruthy();
    expect(screen.getAllByText("a version")).toHaveLength(2);
  });

  it("is reached from a run's workflow, in the runs view and in the inspector", async () => {
    const place = open("/finance/runs");
    const links = await screen.findAllByRole("link", { name: "monthly-invoicing" });
    await fireEvent.click(links[0]!);
    expect(place.route).toEqual({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing" });
  });

  it("answers as the API does for a workflow that is not there, or in a namespace not the caller's", async () => {
    open("/finance/workflows/nothing");
    expect(await screen.findByText("No such thing, or not yours.")).toBeTruthy();
  });
});
