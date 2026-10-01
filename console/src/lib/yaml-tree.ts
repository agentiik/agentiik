import { isMap, isScalar, isSeq, parse, parseDocument, visit, type Document, type Node, type Pair, type Scalar, type YAMLMap } from "yaml";

// The workflow file as a tree that keeps it as it was written. A file is read as YAML 1.2, where on
// is a key and not the boolean YAML 1.1 makes of it, into a tree whose every node knows where in the
// text it was written; an edit replaces the bytes of the node it changes and no other, and the file
// is read again from the result. So a file edited once and saved differs from the file read by the
// edit alone: its comments, its key order, its quoting, its blank lines and the indentation it chose
// are the text's, never a writer's idea of them, and a review sees the change a person made.

export type Path = (string | number)[];

export class Refused extends Error {}

export class YamlTree {
  readonly text: string;
  readonly doc: Document.Parsed;

  constructor(text: string) {
    this.text = text;
    this.doc = parseDocument(text, { version: "1.2", keepSourceTokens: true });
  }

  // problems are what the text cannot be read as, each on its line, before any schema is asked.
  get problems(): { line: number; message: string }[] {
    return this.doc.errors.map((e) => ({ line: e.linePos?.[0].line ?? 1, message: e.message.split("\n")[0]! }));
  }

  // value is the document as data, what a schema checks and what the engine reads.
  value(): unknown {
    return this.doc.toJS();
  }

  // nodeAt is the node a path names, or undefined where none is written.
  nodeAt(path: Path): Node | undefined {
    if (path.length === 0) return (this.doc.contents ?? undefined) as Node | undefined;
    const found = this.doc.getIn(path, true);
    return found && typeof found === "object" ? (found as Node) : undefined;
  }

  // lineOf is the line, from 1, an offset of the text is on.
  lineOf(offset: number): number {
    let line = 1;
    for (let i = 0; i < offset && i < this.text.length; i++) if (this.text.charCodeAt(i) === 10) line++;
    return line;
  }

  // lineAt is the line of the nearest node a path names, going up the path until one is written:
  // where a schema finds a key missing, the map it is missing from is where the person looks.
  lineAt(path: Path): number {
    for (let n = path.length; n > 0; n--) {
      // The line of the key a value is written under, rather than of the value, which for a block
      // map is the line of its first key and not the one naming it.
      const parent = this.nodeAt(path.slice(0, n - 1));
      if (parent && isMap(parent)) {
        const pair = parent.items.find((p) => keyOf(p) === path[n - 1]);
        const key = pair?.key as Node | undefined;
        if (key?.range) return this.lineOf(key.range[0]);
      } else if (parent && isSeq(parent) && typeof path[n - 1] === "number") {
        const item = parent.items[path[n - 1] as number] as Node | undefined;
        if (item?.range) return this.lineOf(item.range[0]);
      }
    }
    return 1;
  }

  // set gives the scalar a path names a new value, written in the quoting it had: the bytes of the
  // old value are replaced, the rest of the line with them stays. A path naming nothing yet in a map
  // that is written adds the key to that map.
  set(path: Path, value: string | number | boolean | null): YamlTree {
    const node = this.nodeAt(path);
    if (node && isScalar(node) && node.range) {
      return this.splice(node.range[0], node.range[1], scalarSource(value, node.type));
    }
    if (node) throw new Refused(`${show(path)} holds a ${isMap(node) ? "map" : isSeq(node) ? "list" : "value"}, which a scalar does not replace`);
    const parent = this.nodeAt(path.slice(0, -1));
    const key = path[path.length - 1];
    if (!parent || !isMap(parent) || typeof key !== "string") throw new Refused(`${show(path.slice(0, -1))} is not a map written in the file`);
    return this.add(parent, key, scalarSource(value));
  }

  // remove takes the key a path names out of its map, with its value and the lines they were written
  // on, and nothing else.
  remove(path: Path): YamlTree {
    const parent = this.nodeAt(path.slice(0, -1));
    const key = path[path.length - 1];
    if (!parent || !isMap(parent)) throw new Refused(`${show(path.slice(0, -1))} is not a map written in the file`);
    const i = parent.items.findIndex((p) => keyOf(p) === key);
    if (i < 0) throw new Refused(`${show(path)} is not written in the file`);
    const pair = parent.items[i]!;
    const start = (pair.key as Node).range![0];
    const end = endOf(pair);
    if (parent.flow) {
      // In a flow map, the pair and the separator on one side of it: the comma after it, or before
      // it where it is the last; the only pair leaves the braces alone.
      if (parent.items.length === 1) return this.splice(parent.range![0], parent.range![1], "{}");
      const after = this.text.slice(end).match(/^\s*,\s*/);
      if (after) return this.splice(start, end + after[0].length, "");
      const before = this.text.slice(0, start).match(/,\s*$/);
      return this.splice(before ? start - before[0].length : start, end, "");
    }
    // In a map whose keys are set apart by blank lines, the blank line before the key goes with
    // it, so that two do not meet where it was.
    let from = lineStart(this.text, start);
    if (from > 0 && this.apart(parent)) {
      const before = lineStart(this.text, from - 1);
      if (this.text.slice(before, from - 1).trim() === "") from = before;
    }
    return this.splice(from, lineEnd(this.text, end) + 1, "");
  }

  // insert writes a key a written map does not hold yet: inline, a scalar or a flow collection on the
  // key's line, as add writes one; or block, lines under the key, each indented by its leading pairs
  // of spaces one step further than the key, in the step the file indents with. A block map whose
  // keys are set apart by a blank line has the new key set apart too.
  insert(mapPath: Path, key: string, value: { inline: string } | { block: string[] }): YamlTree {
    const map = this.nodeAt(mapPath);
    if (!map || !isMap(map)) throw new Refused(`${show(mapPath)} is not a map written in the file`);
    if (map.items.some((p) => keyOf(p) === key)) throw new Refused(`${show([...mapPath, key])} is written already`);
    if ("inline" in value) return this.add(map, key, value.inline);
    if (map.flow) throw new Refused(`${show(mapPath)} is written as a flow map, which a block does not go into`);
    const last = map.items[map.items.length - 1];
    if (!last) throw new Refused("An empty block map has no indentation to follow");
    const first = map.items[0]!.key as Node;
    const indent = " ".repeat(first.range![0] - lineStart(this.text, first.range![0]));
    const unit = this.unit();
    const lines = value.block.map((l) => {
      const depth = Math.floor((l.length - l.trimStart().length) / 2);
      return `
${indent}${unit.repeat(depth + 1)}${l.trimStart()}`;
    });
    const at = lineEnd(this.text, endOf(last));
    const apart = this.apart(map) ? "\n" : "";
    return this.splice(at, at, `${apart}\n${indent}${scalarSource(key)}:${lines.join("")}`);
  }

  // append writes an item at the end of a written list, in the list's own style: on a line of its
  // own beside the dash of the items before it in a block list, after a comma in a flow list.
  append(seqPath: Path, source: string): YamlTree {
    const seq = this.nodeAt(seqPath);
    if (!seq || !isSeq(seq)) throw new Refused(`${show(seqPath)} is not a list written in the file`);
    if (seq.flow) {
      const close = seq.range![1] - 1;
      if (this.text[close] !== "]") throw new Refused("The list is written in a way the editor does not add to");
      const inner = this.text.slice(seq.range![0] + 1, close);
      if (inner.trim() === "") return this.splice(seq.range![0], seq.range![1], `[${source}]`);
      const trailing = inner.match(/\s*$/)![0];
      return this.splice(close - trailing.length, close - trailing.length, `, ${source}`);
    }
    const items = seq.items as Node[];
    const last = items[items.length - 1];
    if (!last?.range) throw new Refused("An empty block list has no indentation to follow");
    const dash = this.text.lastIndexOf("-", items[0]!.range![0]);
    const column = dash - lineStart(this.text, dash);
    const at = lineEnd(this.text, last.range[1]);
    return this.splice(at, at, `\n${" ".repeat(column)}- ${source}`);
  }

  // removeItem takes one item out of a written list, with the line it was written on in a block
  // list, or the comma beside it in a flow list.
  removeItem(seqPath: Path, index: number): YamlTree {
    const seq = this.nodeAt(seqPath);
    if (!seq || !isSeq(seq)) throw new Refused(`${show(seqPath)} is not a list written in the file`);
    const item = seq.items[index] as Node | undefined;
    if (!item?.range) throw new Refused(`${show([...seqPath, index])} is not written in the file`);
    const [start, end] = [item.range[0], item.range[1]];
    if (seq.flow) {
      if (seq.items.length === 1) return this.splice(seq.range![0], seq.range![1], "[]");
      const after = this.text.slice(end).match(/^\s*,\s*/);
      if (after) return this.splice(start, end + after[0].length, "");
      const before = this.text.slice(0, start).match(/,\s*$/);
      return this.splice(before ? start - before[0].length : start, end, "");
    }
    const dash = this.text.lastIndexOf("-", start);
    return this.splice(lineStart(this.text, dash), lineEnd(this.text, end) + 1, "");
  }

  // unit is the step the file indents a block with, from the first block map written under a key
  // of another, two spaces where none is.
  private unit(): string {
    let found = 0;
    visit(this.doc, {
      Pair: (_, pair) => {
        const k = pair.key as Node | null;
        const v = pair.value as Node | null;
        if (found || !k?.range || !v || !isMap(v) || v.flow || !v.items.length) return;
        const child = v.items[0]!.key as Node;
        if (!child?.range) return;
        const diff = column(this.text, child.range[0]) - column(this.text, k.range[0]);
        if (diff > 0) found = diff;
      },
    });
    return " ".repeat(found || 2);
  }

  // apart says whether a block map's keys are set apart by blank lines, as a file's steps often are.
  private apart(map: YAMLMap): boolean {
    if (map.items.length < 2) return false;
    const a = endOf(map.items[0]!);
    const b = (map.items[1]!.key as Node).range![0];
    return /\n[ \t]*\n/.test(this.text.slice(a - 1, b));
  }

  // add writes a new key at the end of a map, in the map's own style: on a line of its own at the
  // indentation of the keys before it in a block map, after a comma in a flow map.
  private add(map: YAMLMap, key: string, source: string): YamlTree {
    const k = scalarSource(key);
    if (map.flow) {
      const close = map.range![1] - 1;
      if (this.text[close] !== "}") throw new Refused("The map is written in a way the editor does not add to");
      const inner = this.text.slice(map.range![0] + 1, close);
      if (inner.trim() === "") return this.splice(map.range![0], map.range![1], `{ ${k}: ${source} }`);
      const trailing = inner.match(/\s*$/)![0];
      return this.splice(close - trailing.length, close - trailing.length, `, ${k}: ${source}`);
    }
    const last = map.items[map.items.length - 1];
    if (!last) throw new Refused("An empty block map has no indentation to follow");
    const first = map.items[0]!.key as Node;
    const indent = " ".repeat(first.range![0] - lineStart(this.text, first.range![0]));
    const at = lineEnd(this.text, endOf(last));
    return this.splice(at, at, `\n${indent}${k}: ${source}`);
  }

  private splice(from: number, to: number, source: string): YamlTree {
    return new YamlTree(this.text.slice(0, from) + source + this.text.slice(to));
  }
}

function keyOf(pair: Pair): unknown {
  return isScalar(pair.key) ? (pair.key as Scalar).value : pair.key;
}

// endOf is where a pair's value ends, before the newline a block value is read with.
function endOf(pair: Pair): number {
  const v = pair.value as Node | null;
  const end = v?.range ? v.range[1] : (pair.key as Node).range![1];
  return end;
}

function column(text: string, offset: number): number {
  return offset - lineStart(text, offset);
}

function lineStart(text: string, offset: number): number {
  return text.lastIndexOf("\n", offset - 1) + 1;
}

// lineEnd is the newline ending the line an offset is on, or the end of the text: an offset just
// past a newline, where a block value ends, is on the line that newline ends.
function lineEnd(text: string, offset: number): number {
  const from = offset > 0 && text[offset - 1] === "\n" ? offset - 1 : offset;
  const i = text.indexOf("\n", from);
  return i < 0 ? text.length : i;
}

// scalarSource is a value as the file would write it, in the quoting given: a string plain where it
// reads back as the same string, and double-quoted where it would not.
export function scalarSource(value: string | number | boolean | null, type?: Scalar.Type | null): string {
  if (value === null) return "null";
  if (typeof value !== "string") return String(value);
  if (type === "QUOTE_SINGLE") return `'${value.replaceAll("'", "''")}'`;
  if (type === "QUOTE_DOUBLE") return JSON.stringify(value);
  if (value !== "" && !/^\s|\s$|\n|: |\s#/.test(value)) {
    try {
      if (parse(value, { version: "1.2" }) === value) return value;
    } catch {
      // not plain, then
    }
  }
  return JSON.stringify(value);
}

function show(path: Path): string {
  return path.length === 0 ? "the document" : path.map(String).join(".");
}
