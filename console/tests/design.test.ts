import { describe, expect, it } from "vitest";

// What the design system rules of every surface, read off the console's own styles: "A shadow says that
// something is above something else, and nothing else", and two shadows are the whole set. A sheet or
// what floats is lifted by shadowSheet or shadowFloat; anything else a box-shadow draws is a ring, its
// offsets and blur all zero, or a hairline drawn inside a box, inset, across a bar or a row.

const styles = import.meta.glob(["../src/**/*.svelte", "../src/**/*.css"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;

// shadows answers every box-shadow a stylesheet declares, by its line, with its value as written.
function shadows(source: string): { line: number; value: string }[] {
  return [...source.matchAll(/box-shadow:\s*([^;]+);/g)].map((m) => ({ line: source.slice(0, m.index).split("\n").length, value: m[1]!.trim() }));
}

// lifts answers whether one layer of a box-shadow casts a shadow below its box: neither inset, a ring
// spread around the box with no offset or blur, nor one of the two depths the design system names.
function lifts(layer: string): boolean {
  const l = layer.trim();
  return !(l === "none" || l.startsWith("inset") || /^var\(--shadow(Sheet|Float)\)$/.test(l) || /^0 0 0 /.test(l));
}

describe("elevation", () => {
  it("is a ground, a ring or one of the two depths, never a shadow of a component's own", () => {
    const found = Object.entries(styles).flatMap(([file, source]) =>
      shadows(source)
        .filter((s) => s.value.split(/,(?![^(]*\))/).some(lifts))
        .map((s) => `${file.replace("../src/", "")}:${s.line} ${s.value}`),
    );
    expect(Object.keys(styles).length).toBeGreaterThan(40);
    expect(found).toEqual([]);
  });

  it("tells a depth and a ring from a shadow of a component's own", () => {
    expect(["var(--shadowSheet)", "var(--shadowFloat)", "0 0 0 1px var(--line)", "inset 0 -1px 0 var(--line)", "none"].filter(lifts)).toEqual([]);
    expect(["0 8px 24px rgb(0 0 0 / 0.12)", "0 1px 2px var(--line)"].filter(lifts)).toHaveLength(2);
  });

  it("is read from every declaration, a hairline drawn inside apart", () => {
    expect(shadows(".a { box-shadow: inset 0 -1px 0 var(--line); }\n.b {\n  box-shadow: 0 8px 24px rgb(0 0 0 / 0.12);\n}")).toEqual([
      { line: 1, value: "inset 0 -1px 0 var(--line)" },
      { line: 3, value: "0 8px 24px rgb(0 0 0 / 0.12)" },
    ]);
  });
});
