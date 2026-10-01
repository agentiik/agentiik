import { describe, expect, it } from "vitest";
import { describeAt, keysAt, placeAt, valuesAt } from "../src/lib/editor/schema-help";

const end = (text: string) => placeAt(text, text.length);

describe("the place a cursor types at", () => {
  it("is a key of the document at its first column", () => {
    expect(end("apiVersion: agentiik.dev/v1\nkind: Workflow\nst")).toEqual({ path: [], key: null, word: "st", from: 43 });
  });

  it("is a key of a step, read from the lines above by their indentation", () => {
    expect(end("steps:\n  invoice:\n    image: x\n    # a note\n\n    ret").path).toEqual(["steps", "invoice"]);
  });

  it("is a key of an item of a list, indented under its key or at its key's own indentation", () => {
    expect(end("steps:\n  invoice:\n    needs:\n      - step: a\n        po").path).toEqual(["steps", "invoice", "needs", []]);
    expect(end("steps:\n  invoice:\n    needs:\n    - step: a\n      po").path).toEqual(["steps", "invoice", "needs", []]);
    expect(end("steps:\n  invoice:\n    needs:\n      - po").path).toEqual(["steps", "invoice", "needs", []]);
  });

  it("is the value of the key on its line", () => {
    expect(end("steps:\n  invoice:\n    merge: wa")).toEqual({ path: ["steps", "invoice"], key: "merge", word: "wa", from: 29 });
  });
});

describe("what the schema says there", () => {
  it("offers the keys a map takes, each with what it is for", () => {
    const top = keysAt([]).map((k) => k.name);
    expect(top).toEqual(expect.arrayContaining(["apiVersion", "kind", "metadata", "inputs", "outputs", "on", "steps"]));
    const step = keysAt(["steps", "invoice"]);
    expect(step.map((k) => k.name)).toEqual(expect.arrayContaining(["image", "needs", "outputs", "merge", "strategy", "retry", "timeout"]));
    expect(step.find((k) => k.name === "image")!.description).not.toBe("");
    expect(keysAt(["steps", "invoice", "needs", []]).map((k) => k.name)).toEqual(expect.arrayContaining(["step", "port", "as"]));
  });

  it("offers the values a key takes where the schema lists them", () => {
    expect(valuesAt(["steps", "invoice", "merge"])).toEqual(expect.arrayContaining(["wait_all", "zip", "first"]));
    expect(valuesAt(["steps", "invoice", "continue_on_error"])).toEqual(expect.arrayContaining(["true", "false"]));
  });

  it("says what a key is for", () => {
    expect(describeAt(["steps"])).toMatch(/steps of the workflow/i);
    expect(describeAt(["nothing-here"])).toBe("");
  });
});
