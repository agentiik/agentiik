import { describe, expect, it } from "vitest";
import { includedPaths, pathOf, resolve } from "../src/lib/editor/resolve";
import { scenario } from "./scenario";

const head = "a3f9c1e04b7d2e8f6a1c3b5d7e9f0a2b4c6d8e0f";
const at = (path: string) => (scenario("alice") as Record<string, { text?: string }>)[`GET /api/v1/finance/workflows/monthly-invoicing/tree/${head}?path=${path}`]?.text;

describe("an include's path", () => {
  it("is read from the directory of the file naming it, or from the root, and never leaves the tree", () => {
    expect(pathOf("agentiik.yaml", "./common-bricks.yaml")).toBe("common-bricks.yaml");
    expect(pathOf("lib/a.yaml", "b.yaml")).toBe("lib/b.yaml");
    expect(pathOf("lib/a.yaml", "../b.yaml")).toBe("b.yaml");
    expect(pathOf("lib/a.yaml", "/c/d.yaml")).toBe("c/d.yaml");
    expect(pathOf("agentiik.yaml", "../outside.yaml")).toBeUndefined();
  });
});

describe("a workflow resolved in the browser", () => {
  it("reads its path includes, and resolves monthly-invoicing as the API does where the graph is concerned", () => {
    const entry = at("agentiik.yaml")!;
    expect(includedPaths(entry, at)).toEqual(["common-bricks.yaml"]);
    const base = (scenario("alice")["GET /api/v1/finance/workflows/monthly-invoicing"]!.body as { graph: never }).graph;
    const { graph, written, problems } = resolve(entry, at, base);
    expect(problems).toEqual([]);
    expect(graph.order).toEqual(["normalize", "invoice", "archive"]);
    expect(graph.steps.invoice!.needs).toEqual([{ step: "normalize", port: "ok", as: "in" }]);
    expect(graph.steps.archive!.needs).toEqual([
      { step: "invoice", port: "out", as: "invoices" },
      { step: "normalize", port: "rejected", as: "rejected" },
    ]);
    expect(graph.steps.invoice!.strategy).toEqual({ fan_out: "item", max_parallel: 8 });
    // The block it extends gives it what it does not write: the network, and the timeout.
    expect(graph.steps.invoice!.network).toBe("egress");
    expect(graph.steps.invoice!.extends).toEqual([".api-brick"]);
    expect(written.get("invoice")).toBe("entry");
  });

  it("merges every layer as the language does: blocks outermost first, then defaults, then the step's own", () => {
    const files: Record<string, string> = {
      "agentiik.yaml": [
        "include: [{ path: lib/base.yaml }]",
        "defaults: { timeout: 5m, before_script: [d] }",
        ".child: { extends: .root, params: { b: 2 }, before_script: [c] }",
        "steps:",
        "  run:",
        "    extends: .child",
        "    image: x",
        "    params: { c: 3 }",
        "    before_script: [own]",
        "  shared:",
        "    retry: { max: 1 }",
      ].join("\n"),
      "lib/base.yaml": [".root: { timeout: 1m, params: { a: 1, b: 1 }, before_script: [r], retry: { max: 9 } }", "steps:", "  shared: { image: y, timeout: 2m }", "vars: { v: 1 }", "secrets: [s]"].join("\n"),
    };
    const { graph, written, problems } = resolve(files["agentiik.yaml"]!, (p) => files[p]);
    expect(problems).toEqual([]);
    const run = graph.steps.run as Record<string, unknown>;
    expect(run.timeout).toBe("5m");
    expect(run.params).toEqual({ a: 1, b: 2, c: 3 });
    expect(run.retry).toEqual({ max: 9 });
    expect(run.before_script).toEqual(["d", "r", "c", "own"]);
    expect(run.extends).toEqual([".child", ".root"]);
    // A step an included file writes, which the entry point overrides keyword by keyword.
    expect(graph.steps.shared).toMatchObject({ image: "y", timeout: "2m", retry: { max: 1 } });
    expect(written.get("shared")).toBe("entry");
    expect(graph.vars).toEqual({ v: 1 });
    expect(graph.secrets).toEqual(["s"]);
    expect(graph.order).toEqual(["shared", "run"]);
  });

  it("says what it cannot resolve, and draws what it cannot read as the version resolved it", () => {
    const entry = ["include:", "  - { workflow: finance/common, ref: v2.1.0 }", "  - { path: gone.yaml }", "steps:", "  a: { extends: .nowhere, image: x, needs: [b] }", "  b: { image: x, needs: [a] }"].join("\n");
    const base = { workflow: "w", commit: head, includes: [], order: ["lib"], steps: { lib: { kind: "brick", image: "z" } } } as never;
    const { graph, problems, written } = resolve(entry, () => undefined, base);
    expect(problems).toEqual([
      "finance/common at v2.1.0 is included from another repository, whose steps are drawn as the version resolves them.",
      "gone.yaml, which agentiik.yaml includes, could not be read; what it gives is drawn as the version resolves it.",
      "a extends .nowhere, which no file writes.",
      "a, b need one another, which no run can start.",
    ]);
    expect(graph.steps.lib).toEqual({ kind: "brick", image: "z" });
    expect(written.get("lib")).toBe("included");
  });
});
