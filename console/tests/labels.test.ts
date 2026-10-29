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
// With every, it answers every button and link that is no entry of a list, whatever its class.
function controls(source: string, every = false): { line: number; label: string; red: boolean }[] {
  const found: { line: number; label: string; red: boolean }[] = [];
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
      if (close === -1 || (every ? entry(source, at, open) : !/class="[^"]*\bcontrol\b/.test(open))) continue;
      const label = source.slice(end + 1, close).replace(/<[^>]*>/g, "").replace(/\s+/g, " ").trim();
      found.push({ line: source.slice(0, at).split("\n").length, label, red: /class="[^"]*\bdanger\b|class:danger=/.test(open) });
    }
  }
  return found;
}

// entry says whether the button or link opening at at is an entry of a list, which names its item:
// one inside a list's item or a table's row, or a node of the graph.
function entry(source: string, at: number, open: string): boolean {
  const before = source.slice(0, at);
  const inside = (tag: string) => Math.max(before.lastIndexOf(`<${tag} `), before.lastIndexOf(`<${tag}>`)) > before.lastIndexOf(`</${tag}>`);
  return inside("li") || inside("tr") || /class="node\b/.test(open);
}

// mixed says whether a label holds a value beside words of its own: a value is an expression, and a
// block's tags, {#if}, {:else} and {/if}, are none, the words they hold being the label's.
function mixed(label: string): boolean {
  let words = "";
  let value = false;
  for (let i = 0; i < label.length; i++) {
    if (label[i] !== "{") {
      words += label[i];
      continue;
    }
    if (!"#:/@".includes(label[i + 1] ?? "")) value = true;
    for (let depth = 0; i < label.length; i++) {
      if (label[i] === "{") depth++;
      else if (label[i] === "}" && --depth === 0) break;
    }
  }
  return value && /[A-Za-z]/.test(words);
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
    expect(controls('<p><button class="control danger" onclick={() => go(a > b)}><Icon name="x" size={14} />Remove {name}</button></p>')).toEqual([{ line: 1, label: "Remove {name}", red: true }]);
    expect(controls('<button class="step" onclick={() => pick()}>{step}</button>')).toEqual([]);
  });

  // A link or a button of any class that names an item holds its name alone, and one that says what it
  // does holds words alone: words beside a value are a control's label that moves with the value.
  it("holds words or a value in every link and button that is no entry of a list, never both", () => {
    const found = Object.entries(sources).flatMap(([file, source]) =>
      controls(source, true)
        .filter((c) => mixed(c.label))
        .map((c) => `${file.replace("../src/", "")}:${c.line} ${c.label}`),
    );
    expect(found).toEqual([]);
  });

  it("is read from every link and button with every, a list's entries and a graph's nodes aside", () => {
    expect(controls('<p class="open"><a href={x}>Open {step} in the {i === 0 ? "first" : "second"} run</a></p>', true).filter((c) => mixed(c.label))).toHaveLength(1);
    expect(controls('<li><button class="line" onclick={go}>line {p.line}</button></li>', true)).toEqual([]);
    expect(controls('<tr><td><button class="link">{t.shard ? `${t.shard.index}/${t.shard.of} · ` : ""}attempt {t.attempt}</button></td></tr>', true)).toEqual([]);
    expect(controls('<button class="node {st}">{p.step} of {n}</button>', true)).toEqual([]);
    expect(mixed("{t.label}{#if t.count !== undefined}{t.count}{/if}")).toBe(false);
    expect(mixed("Search{#if !folded}:{/if}")).toBe(false);
    expect(mixed("{namespace}/{wf}")).toBe(false);
  });
});

// A confirmation draws Keep before the same button turned red, so that the button clicked stays where
// it was and the second click lands on it: Keep is drawn in the room before it, at the end of a row
// whose acts sit at its end, and never after it, nor before a button that is not the one asked about.
describe("a confirmation", () => {
  it("draws Keep before the button it asks about, turned red", () => {
    const found = Object.entries(sources).flatMap(([file, source]) => {
      const drawn = controls(source).sort((a, b) => a.line - b.line);
      return drawn.flatMap((c, i) => (c.label === "Keep" && drawn[i + 1]?.red ? [] : c.label.startsWith("Keep") ? [`${file.replace("../src/", "")}:${c.line} ${c.label} before ${drawn[i + 1]?.label ?? "nothing"}`] : []));
    });
    expect(found).toEqual([]);
  });
});

// What an act answers is never drawn above the button that asked for it, where it would push the
// button down under the pointer: a form's refusal is drawn under its buttons, a dialog's under its
// form, and what an act did is said at the foot of the screen.
describe("an act's answer", () => {
  // regions answers each stretch of a component from an opening tag to its closing one.
  const regions = (source: string, open: string, close: string) => {
    const out: { line: number; text: string }[] = [];
    for (let at = source.indexOf(open); at !== -1; at = source.indexOf(open, at + 1)) {
      const end = source.indexOf(close, at);
      if (end !== -1) out.push({ line: source.slice(0, at).split("\n").length, text: source.slice(at, end) });
    }
    return out;
  };

  it("is drawn under the button of the form or the dialog it answers", () => {
    const found = Object.entries(sources).flatMap(([file, source]) => {
      const where = file.replace("../src/", "");
      const forms = regions(source, "<form", "</form>").flatMap((r) => {
        const last = r.text.lastIndexOf("<button");
        const above = [r.text.indexOf("<Problem"), r.text.indexOf('role="status"')].filter((i) => i !== -1 && i < last);
        return above.length ? [`${where}:${r.line} form`] : [];
      });
      const dialogs = regions(source, "<Dialog", "</Dialog>").flatMap((r) => {
        const form = r.text.indexOf("<form");
        const problem = r.text.indexOf("<Problem");
        return form !== -1 && problem !== -1 && problem < form ? [`${where}:${r.line} dialog`] : [];
      });
      return [...forms, ...dialogs];
    });
    expect(found).toEqual([]);
  });
});
