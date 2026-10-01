import { describe, expect, it } from "vitest";
import { isMap, isScalar } from "yaml";
import { lineDiff } from "../src/lib/tree";
import { Refused, scalarSource, YamlTree, type Path } from "../src/lib/yaml-tree";
import { check } from "../src/lib/workflow-check";

// The round-trip corpus: every workflow file the schemas repository pins what it accepts and refuses
// with, copied as it publishes them. Each is read and edited, and the build fails on any byte that
// changes beyond the edit.
const corpus = import.meta.glob("./corpus/*.yaml", { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const index = (await import("./corpus/index.json")).default as { fixtures: { workflow: { invalid: { file: string; refused_by: string }[] } } };

// changed is how many lines an edit removed and added.
function changed(before: string, after: string) {
  const d = lineDiff(before, after);
  return { removed: d.filter((l) => l.kind === "removed").length, added: d.filter((l) => l.kind === "added").length };
}

// aStep is a step of the file written as a block map with an image, the edit the corpus is held to.
function aStep(tree: YamlTree): string | undefined {
  const steps = tree.nodeAt(["steps"]);
  if (!steps || !isMap(steps) || steps.flow) return undefined;
  for (const pair of steps.items) {
    const name = isScalar(pair.key) ? String(pair.key.value) : undefined;
    const step = pair.value;
    if (name && step && isMap(step) && !step.flow && isScalar(step.get("image", true))) return name;
  }
  return undefined;
}

describe("the round-trip corpus", () => {
  const files = Object.entries(corpus);

  it("holds the schemas repository's workflow files", () => {
    expect(files.length).toBeGreaterThan(80);
  });

  for (const [name, text] of files) {
    it(`changes nothing in ${name.replace("./corpus/", "")} beyond the edit`, () => {
      const tree = new YamlTree(text);
      expect(tree.text).toBe(text);
      const step = aStep(tree);
      if (tree.problems.length > 0 || !step) return;
      const value = tree.value() as { steps: Record<string, Record<string, unknown>> };

      // A value replaced is one line removed and one added, and the data differs there alone.
      const image: Path = ["steps", step, "image"];
      const was = value.steps[step]!.image as string;
      const set = tree.set(image, "registry.example.com/acme/edited:1.0.0");
      expect(changed(text, set.text)).toEqual({ removed: 1, added: 1 });
      const after = set.value() as typeof value;
      expect(after.steps[step]!.image).toBe("registry.example.com/acme/edited:1.0.0");
      after.steps[step]!.image = was;
      expect(after).toEqual(value);
      // And set back, the file is the file again, byte for byte.
      expect(set.set(image, was).text).toBe(text);

      // A key added is one line, and taken out again leaves the file as it was.
      const timeout: Path = ["steps", step, "timeout_added_by_the_corpus"];
      const added = tree.set(timeout, "5m");
      expect(changed(text, added.text)).toEqual({ removed: 0, added: 1 });
      expect((added.value() as typeof value).steps[step]!["timeout_added_by_the_corpus"]).toBe("5m");
      expect(added.remove(timeout).text).toBe(text);
    });
  }
});

describe("the tree", () => {
  const file = `# finance/monthly-invoicing
apiVersion: agentiik.dev/v1
kind: Workflow

on:
  schedule:
    - cron: "0 6 1 * *"   # the first of the month

steps:
  normalize:
    image: 'ghcr.io/acme/agk-normalize:1.4.0'
    params: { currency: EUR }
    outputs: [ok, rejected]
  invoice:
    image: ghcr.io/acme/agk-invoice:3.2.1
    params:
      rounding: per_line
`;

  it("reads on as a key, YAML 1.2's reading, and says where a node is written", () => {
    const tree = new YamlTree(file);
    expect(Object.keys(tree.value() as object)).toContain("on");
    expect(tree.lineAt(["steps", "invoice", "params", "rounding"])).toBe(17);
    expect(tree.lineAt(["steps", "invoice", "missing", "deeper"])).toBe(14);
  });

  it("keeps a value's quoting, and a comment on its line", () => {
    const tree = new YamlTree(file);
    const quoted = tree.set(["steps", "normalize", "image"], "ghcr.io/acme/agk-normalize:1.5.0");
    expect(quoted.text).toContain("    image: 'ghcr.io/acme/agk-normalize:1.5.0'\n");
    const cron = tree.set(["on", "schedule", 0, "cron"], "0 7 1 * *");
    expect(cron.text).toContain(`    - cron: "0 7 1 * *"   # the first of the month\n`);
  });

  it("adds to a flow map after a comma, and to an empty one between its braces", () => {
    const tree = new YamlTree(file);
    expect(tree.set(["steps", "normalize", "params", "region"], "eu-west").text).toContain("    params: { currency: EUR, region: eu-west }\n");
    const empty = new YamlTree("steps:\n  a:\n    image: x\n    params: {}\n");
    expect(empty.set(["steps", "a", "params", "n"], 3).text).toBe("steps:\n  a:\n    image: x\n    params: { n: 3 }\n");
  });

  it("takes a key out of a block map with the lines of its value, and of a flow map with its comma", () => {
    const tree = new YamlTree(file);
    const without = tree.remove(["steps", "invoice", "params"]);
    expect(without.text).toBe(file.replace("    params:\n      rounding: per_line\n", ""));
    expect(tree.remove(["steps", "normalize", "params", "currency"]).text).toContain("    params: {}\n");
    const two = new YamlTree("a: { x: 1, y: 2 }\n");
    expect(two.remove(["a", "y"]).text).toBe("a: { x: 1 }\n");
    expect(two.remove(["a", "x"]).text).toBe("a: { y: 2 }\n");
  });

  it("refuses an edit it cannot make without rewriting what it does not change", () => {
    const tree = new YamlTree(file);
    expect(() => tree.set(["steps", "invoice"], "x")).toThrow(Refused);
    expect(() => tree.set(["nowhere", "deeper"], "x")).toThrow(/nowhere is not a map written in the file/);
    expect(() => tree.remove(["steps", "absent"])).toThrow(/steps\.absent is not written in the file/);
  });

  it("says what the text cannot be read as, on its line", () => {
    expect(new YamlTree("a: 1\nb: [\n").problems[0]!.line).toBeGreaterThan(1);
    expect(new YamlTree("a: 1\na: 2\n").problems).toHaveLength(1);
  });

  it("writes a string plain where it reads back the same, and quoted where it would not", () => {
    expect(scalarSource("per_line")).toBe("per_line");
    expect(scalarSource("true")).toBe('"true"');
    expect(scalarSource("8")).toBe('"8"');
    expect(scalarSource("a: b")).toBe('"a: b"');
    expect(scalarSource("${{ vars.x }}")).toBe("${{ vars.x }}");
    expect(scalarSource("it's", "QUOTE_SINGLE")).toBe("'it''s'");
    expect(scalarSource(8)).toBe("8");
  });
});

describe("the check against workflow.schema.json", () => {
  const ok = Object.entries(corpus).filter(([n]) => n.includes("-valid-") && n.includes("workflow-"));

  it("finds nothing wrong in a file the schemas accept", () => {
    expect(ok.length).toBeGreaterThan(10);
    for (const [name, text] of ok) expect([name, check(new YamlTree(text))]).toEqual([name, []]);
  });

  // Which check refuses each file is the schemas repository's to say, in its index: the schema, or
  // a rule over the whole graph the API applies after it, a cycle or a port no image declares, which
  // a file valid against the schema still breaks and the browser leaves to the API.
  it("refuses every file the schemas refuse with the schema itself, on a line of it", () => {
    const listed = index.fixtures.workflow.invalid.filter((f) => f.refused_by === "schema").map((f) => `./corpus/workflow-invalid-${f.file.split("/").pop()}`);
    const refused = Object.entries(corpus).filter(([n]) => listed.includes(n));
    expect(refused.length).toBeGreaterThan(40);
    for (const [name, text] of refused) {
      const tree = new YamlTree(text);
      const problems = check(tree);
      expect([name, problems.length > 0]).toEqual([name, true]);
      for (const p of problems) expect(p.line).toBeLessThanOrEqual(text.split("\n").length);
    }
  });

  it("says a value of the wrong type and a key nothing takes on their lines, and nothing more", () => {
    const tree = new YamlTree("apiVersion: agentiik.dev/v1\nkind: Workflow\nmetadata:\n  name: x\nsteps:\n  a:\n    image: 3\n    bogus: 1\n    outputs: [ok]\n");
    expect(check(tree)).toEqual([
      { line: 7, at: "steps.a.image", message: 'Instance type "number" is invalid. Expected "string".' },
      { line: 8, at: "steps.a.bogus", message: "bogus is not a key steps.a takes" },
    ]);
  });

  it("says what the text cannot be read as before any schema", () => {
    expect(check(new YamlTree("steps: [\n"))[0]!.at).toBe("");
  });
});
