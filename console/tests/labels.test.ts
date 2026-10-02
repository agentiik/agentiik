import { describe, expect, it } from "vitest";

// A button's words never change. A name, a count or a word chosen by a state in its label makes the
// button wider or narrower as the value changes, and moves whatever is drawn beside it; what a
// button acts on is said by where it sits, its row, its pane or the question before it. A button
// that is itself an item of a list, a step of the graph, a file or a port, is not a control and
// names its item.

const sources = import.meta.glob("../src/**/*.svelte", { query: "?raw", import: "default", eager: true }) as Record<string, string>;

// controls answers every button and link drawn as a control in a component, with the words of its
// label as written: its markup and its icons taken out, its expressions left in. An opening tag ends
// at the first > outside braces, since an attribute's expression may hold one, as an arrow does.
function controls(source: string): { line: number; label: string }[] {
  const found: { line: number; label: string }[] = [];
  for (const name of ["button", "a"]) {
    for (let at = source.indexOf(`<${name}`); at !== -1; at = source.indexOf(`<${name}`, at + 1)) {
      if (!/\s/.test(source[at + name.length + 1] ?? "")) continue;
      let depth = 0;
      let end = at;
      for (; end < source.length; end++) {
        const c = source[end];
        if (c === "{") depth++;
        else if (c === "}") depth--;
        else if (c === ">" && depth === 0) break;
      }
      const open = source.slice(at, end);
      const close = source.indexOf(`</${name}>`, end);
      if (close === -1 || !/class="[^"]*\bcontrol\b/.test(open)) continue;
      const label = source.slice(end + 1, close).replace(/<[^>]*>/g, "").replace(/\s+/g, " ").trim();
      found.push({ line: source.slice(0, at).split("\n").length, label });
    }
  }
  return found;
}

describe("a control's label", () => {
  it("holds no expression, so that the control is as wide whatever is shown", () => {
    const found = Object.entries(sources).flatMap(([file, source]) =>
      controls(source)
        .filter((c) => c.label.includes("{"))
        .map((c) => `${file.replace("../src/", "")}:${c.line} ${c.label}`),
    );
    expect(Object.keys(sources).length).toBeGreaterThan(40);
    expect(found).toEqual([]);
  });

  it("is read from every control, its icon and markup aside", () => {
    expect(controls('<p><button class="control danger" onclick={() => go(a > b)}><Icon name="x" size={14} />Remove {name}</button></p>')).toEqual([{ line: 1, label: "Remove {name}" }]);
    expect(controls('<button class="step" onclick={() => pick()}>{step}</button>')).toEqual([]);
  });
});
