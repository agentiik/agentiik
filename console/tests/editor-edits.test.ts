import { describe, expect, it } from "vitest";
import { lineDiff } from "../src/lib/tree";
import { addStep, connect, disconnect, edgesWritten, removeStep, setFanOut, setMaxParallel, setMerge } from "../src/lib/editor/edits";
import { resolve } from "../src/lib/editor/resolve";
import { Refused, YamlTree } from "../src/lib/yaml-tree";
import { scenario } from "./scenario";

const head = "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f";
const at = (path: string) => (scenario("alice") as Record<string, { text?: string }>)[`GET /api/v1/finance/workflows/monthly-invoicing/tree/${head}?path=${path}`]?.text;
const entry = at("agentiik.yaml")!;

// diff is what an edit added and removed, line by line, which is all a review sees of it.
function diff(before: string, after: string) {
  const d = lineDiff(before, after);
  return { added: d.filter((l) => l.kind === "added").map((l) => l.text), removed: d.filter((l) => l.kind === "removed").map((l) => l.text) };
}

const graphOf = (tree: YamlTree) => resolve(tree.text, at).graph;

describe("an edit of the graph", () => {
  it("adds a step at the end of steps, set apart and indented as the file sets its steps", () => {
    const before = new YamlTree(entry);
    const after = addStep(before, graphOf(before), "notify", "ghcr.io/acme/agk-notify:1.0.0", ["ok"]);
    expect(diff(entry, after.text)).toEqual({ added: ["", "  notify:", "    image: ghcr.io/acme/agk-notify:1.0.0", "    outputs: [ok]"], removed: [] });
    expect(graphOf(after).order).toContain("notify");
    expect(after.problems).toEqual([]);
  });

  it("connects a port, appending to needs in the style it is written, and writing needs where there is none", () => {
    const tree = new YamlTree(entry);
    const appended = connect(tree, graphOf(tree), { step: "normalize", port: "ok" }, { step: "archive", as: "normalized" });
    expect(diff(entry, appended.text)).toEqual({ added: ["      - { step: normalize, port: ok, as: normalized }"], removed: [] });
    expect(edgesWritten(appended, "archive").map((e) => e.as)).toEqual(["invoices", "rejected", "normalized"]);

    const added = addStep(tree, graphOf(tree), "notify", "x", ["ok"]);
    const wired = connect(added, graphOf(added), { step: "archive", port: "ok" }, { step: "notify", as: "in" });
    expect(diff(added.text, wired.text)).toEqual({ added: ["    needs:", "      - { step: archive, port: ok, as: in }"], removed: [] });
    expect(graphOf(wired).order).toEqual(["normalize", "invoice", "archive", "notify"]);
  });

  it("refuses an edge the language would refuse, saying why", () => {
    const tree = new YamlTree(entry);
    const g = graphOf(tree);
    const refusal = (f: () => unknown) => {
      try {
        f();
      } catch (e) {
        expect(e).toBeInstanceOf(Refused);
        return (e as Error).message;
      }
      return "nothing refused";
    };
    expect(refusal(() => connect(tree, g, { step: "normalize", port: "nothing" }, { step: "archive", as: "x" }))).toBe("normalize publishes no port nothing");
    expect(refusal(() => connect(tree, g, { step: "archive", port: "ok" }, { step: "normalize", as: "x" }))).toMatch(/would make a cycle/);
    expect(refusal(() => connect(tree, g, { step: "invoice", port: "out" }, { step: "archive", as: "invoices" }))).toBe("archive has that edge already");
    expect(refusal(() => connect(tree, g, { step: "invoice", port: "out" }, { step: "archive", as: "two words" }))).toMatch(/not a name the language takes/);
    expect(refusal(() => removeStep(tree, g, "normalize"))).toBe("invoice, archive need normalize: take those edges away first");
    expect(refusal(() => removeStep(tree, g, "archive"))).toBe("the workflow's invoices is read from archive");
    expect(refusal(() => addStep(tree, g, "invoice", "x", ["ok"]))).toBe("the workflow has a step named invoice already");
  });

  it("takes an edge away, and needs with its last edge", () => {
    const tree = new YamlTree(entry);
    const one = disconnect(tree, "archive", 1);
    expect(diff(entry, one.text)).toEqual({ added: [], removed: ["      - { step: normalize, port: rejected, as: rejected }"] });
    const none = disconnect(tree, "invoice", 0);
    expect(diff(entry, none.text)).toEqual({ added: [], removed: ["    needs:", "      - { step: normalize, port: ok, as: in }"] });
  });

  it("changes a merge, a fan_out and a max_parallel, leaving the defaults unwritten", () => {
    const tree = new YamlTree(entry);
    expect(diff(entry, setMerge(tree, "archive", "zip").text)).toEqual({ added: ["    merge: zip"], removed: ["    merge: wait_all"] });
    expect(diff(entry, setMerge(tree, "archive", "wait_all").text)).toEqual({ added: [], removed: [] });
    expect(diff(entry, setMerge(tree, "normalize", "first").text)).toEqual({ added: ["    merge: first"], removed: [] });
    expect(diff(entry, setMaxParallel(tree, "invoice", 4).text)).toEqual({ added: ["      max_parallel: 4"], removed: ["      max_parallel: 8"] });
    expect(diff(entry, setFanOut(tree, "invoice", "batch(50)").text)).toEqual({ added: ["      fan_out: batch(50)"], removed: ["      fan_out: item"] });
    expect(diff(entry, setFanOut(tree, "invoice", "none").text)).toEqual({ added: ["      fan_out: none"], removed: ["      fan_out: item"] });
    expect(diff(entry, setFanOut(tree, "normalize", "none").text)).toEqual({ added: [], removed: [] });
    // The bound lifted where it is the strategy's last key takes the strategy with it.
    const lifted = setMaxParallel(addStep(tree, graphOf(tree), "notify", "x", ["ok"]), "notify", 3);
    expect(diff(entry, setMaxParallel(lifted, "notify", null).text)).toEqual({ added: ["", "  notify:", "    image: x", "    outputs: [ok]"], removed: [] });
    expect(diff(entry, setMaxParallel(tree, "invoice", null).text)).toEqual({ added: [], removed: ["      max_parallel: 8"] });
    expect(diff(entry, setFanOut(tree, "normalize", "item").text)).toEqual({ added: ["    strategy:", "      fan_out: item"], removed: [] });
    expect(() => setFanOut(tree, "normalize", "batch(0)")).toThrow(Refused);
  });

  it("removes a step nothing needs, with its lines and the blank line setting it apart", () => {
    const tree = new YamlTree(entry);
    const added = addStep(tree, graphOf(tree), "notify", "x", ["ok"]);
    const removed = removeStep(added, graphOf(added), "notify");
    expect(diff(entry, removed.text).added).toEqual([]);
  });
});
