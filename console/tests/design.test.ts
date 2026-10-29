import { describe, expect, it } from "vitest";

// What the design system rules of every surface, read off the console's own styles: "Elevation is
// ground value plus a hairline, never a shadow". A dialog, a menu or a toast is lifted by the
// surface it is drawn on and its outline; a box-shadow drawn inside a box, inset, is a hairline
// across a bar or a row, kept inside it so that what is centred in it is centred in its height.

const styles = import.meta.glob(["../src/**/*.svelte", "../src/**/*.css"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;

// shadows answers every box-shadow a stylesheet declares, by its line, with its value as written.
function shadows(source: string): { line: number; value: string }[] {
  return [...source.matchAll(/box-shadow:\s*([^;]+);/g)].map((m) => ({ line: source.slice(0, m.index).split("\n").length, value: m[1]!.trim() }));
}

describe("elevation", () => {
  it("is a ground and a hairline, never a shadow cast below a box", () => {
    const found = Object.entries(styles).flatMap(([file, source]) =>
      shadows(source)
        .filter((s) => s.value !== "none" && !s.value.startsWith("inset"))
        .map((s) => `${file.replace("../src/", "")}:${s.line} ${s.value}`),
    );
    expect(Object.keys(styles).length).toBeGreaterThan(40);
    expect(found).toEqual([]);
  });

  it("is read from every declaration, a hairline drawn inside apart", () => {
    expect(shadows(".a { box-shadow: inset 0 -1px 0 var(--line); }\n.b {\n  box-shadow: 0 8px 24px rgb(0 0 0 / 0.12);\n}")).toEqual([
      { line: 1, value: "inset 0 -1px 0 var(--line)" },
      { line: 3, value: "0 8px 24px rgb(0 0 0 / 0.12)" },
    ]);
  });
});
