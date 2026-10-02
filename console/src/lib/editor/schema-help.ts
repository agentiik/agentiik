import schema from "../../../vendor/workflow.schema.json";

// What workflow.schema.json says at a place of a file being typed: the keys a map there takes, the
// values a key takes where the schema lists them, and what a key is for. The file is often half
// written while it is typed, which no parser reads, so the place is read from the lines above the
// cursor, by their indentation, as a person reads it.

type Schema = {
  $ref?: string;
  description?: string;
  title?: string;
  type?: string | string[];
  enum?: unknown[];
  const?: unknown;
  properties?: Record<string, Schema>;
  patternProperties?: Record<string, Schema>;
  additionalProperties?: Schema | boolean;
  items?: Schema;
  oneOf?: Schema[];
  anyOf?: Schema[];
  allOf?: Schema[];
};

const root = schema as unknown as Schema & { $defs: Record<string, Schema> };

// A step of the path: a key of a map, or [] for an item of a list.
export type Step = string | [];

function deref(s: Schema | undefined, seen = new Set<string>()): Schema | undefined {
  if (!s?.$ref) return s;
  if (seen.has(s.$ref)) return undefined;
  seen.add(s.$ref);
  const name = s.$ref.replace(/^#\/\$defs\//, "");
  const target = s.$ref === "#" ? root : root.$defs[name];
  // A $ref beside a description keeps the description of the place it is used at.
  const resolved = deref(target, seen);
  return resolved && s.description ? { ...resolved, description: s.description } : resolved;
}

// alternatives is every schema a value may be held to, its oneOf, anyOf and allOf opened.
function alternatives(s: Schema | undefined): Schema[] {
  const d = deref(s);
  if (!d) return [];
  const out: Schema[] = [d];
  for (const k of ["oneOf", "anyOf", "allOf"] as const) for (const x of d[k] ?? []) out.push(...alternatives(x));
  return out;
}

function below(s: Schema, step: Step): Schema[] {
  const out: Schema[] = [];
  for (const a of alternatives(s)) {
    if (Array.isArray(step)) {
      if (a.items) out.push(a.items);
      continue;
    }
    const named = a.properties?.[step];
    if (named) {
      out.push(named);
      continue;
    }
    const pattern = Object.entries(a.patternProperties ?? {}).find(([p]) => new RegExp(p, "u").test(step));
    if (pattern) out.push(pattern[1]);
    else if (a.additionalProperties && typeof a.additionalProperties === "object") out.push(a.additionalProperties);
  }
  return out;
}

// at is the schemas the value at path may be held to.
export function at(path: Step[]): Schema[] {
  let here: Schema[] = [root];
  for (const step of path) here = here.flatMap((s) => below(s, step));
  return here;
}

export type Key = { name: string; description: string };

// keysAt is the keys a map at path takes by name, each with what it is for, in the schema's order.
export function keysAt(path: Step[]): Key[] {
  const out = new Map<string, string>();
  for (const s of at(path)) for (const a of alternatives(s)) for (const [name, p] of Object.entries(a.properties ?? {})) if (!out.has(name)) out.set(name, describe(p));
  return [...out].map(([name, description]) => ({ name, description }));
}

// valuesAt is the values the schema lists for path, an enum's or a boolean's.
export function valuesAt(path: Step[]): string[] {
  const out = new Set<string>();
  for (const s of at(path))
    for (const a of alternatives(s)) {
      for (const v of a.enum ?? []) if (typeof v === "string" || typeof v === "number" || typeof v === "boolean") out.add(String(v));
      if (typeof a.const === "string") out.add(a.const);
      if (a.type === "boolean" || (Array.isArray(a.type) && a.type.includes("boolean"))) ["true", "false"].forEach((v) => out.add(v));
    }
  return [...out];
}

export function describe(s: Schema | undefined): string {
  const d = deref(s);
  return (s?.description ?? d?.description ?? d?.title ?? "").trim();
}

// describeAt is what the key at the end of path is for.
export function describeAt(path: Step[]): string {
  for (const s of at(path)) {
    const said = describe(s);
    if (said) return said;
  }
  return "";
}

type Line = { indent: number; dash: boolean; content: number; key: string | null; rest: string };

const keyed = /^([A-Za-z0-9_.$-]+|"[^"]*"|'[^']*')\s*:(\s|$)/;

function readLine(text: string): Line {
  const indent = text.length - text.trimStart().length;
  let body = text.slice(indent);
  let content = indent;
  let dash = false;
  if (body === "-" || body.startsWith("- ")) {
    dash = true;
    const after = body.slice(1);
    const pad = after.length - after.trimStart().length;
    content = indent + 1 + pad;
    body = after.trimStart();
  }
  const m = keyed.exec(body);
  const key = m ? m[1]!.replace(/^["']|["']$/g, "") : null;
  return { indent, dash, content, key, rest: m ? body.slice(m[0].length).trim() : body };
}

// Place is where the cursor is: the path of the map it types in, and whether it is typing a key, or
// the value of the key named, and the word typed so far.
export type Place = { path: Step[]; key: string | null; word: string; from: number };

// placeAt reads the place of a cursor at pos in text.
export function placeAt(text: string, pos: number): Place {
  const start = text.lastIndexOf("\n", pos - 1) + 1;
  const typed = text.slice(start, pos);
  const line = readLine(typed);
  const inValue = line.key !== null;
  const word = (inValue ? line.rest : typed.slice(line.content)).replace(/^["']/, "");
  const from = pos - word.length;
  // The cursor's map is the one its key sits at; above it, each line indented less is an ancestor.
  let limit = line.content;
  const path: Step[] = [];
  if (line.dash) {
    path.push([]);
    limit = line.indent;
  }
  const lines = text.slice(0, start).split("\n").reverse();
  let listLimit = -1;
  for (const raw of lines) {
    if (!raw.trim() || raw.trimStart().startsWith("#")) continue;
    const l = readLine(raw);
    if (l.dash && l.content === limit) {
      // The cursor's map is this list item's.
      path.push([]);
      limit = l.indent;
      listLimit = l.indent;
      continue;
    }
    if (l.dash && l.content < limit) {
      if (l.key && l.rest === "") path.push(l.key);
      path.push([]);
      limit = l.indent;
      listLimit = l.indent;
      continue;
    }
    // A list may sit at its key's own indentation.
    if (!l.dash && l.key && (l.indent < limit || (l.indent === listLimit && l.indent === limit))) {
      path.push(l.key);
      limit = l.indent;
      listLimit = -1;
    }
    if (limit === 0 && !l.dash && l.indent === 0) break;
  }
  path.reverse();
  return { path, key: inValue ? line.key : null, word, from };
}

// siblingsAt is the keys the map a cursor types in already holds, above and below its line, so that
// a key is not offered twice.
export function siblingsAt(text: string, pos: number): Set<string> {
  const start = text.lastIndexOf("\n", pos - 1) + 1;
  const lines = text.split("\n");
  const at = text.slice(0, start).split("\n").length - 1;
  const own = readLine(text.slice(start, pos));
  const col = own.content;
  const held = new Set<string>();
  const walk = (step: number) => {
    for (let i = at + step; i >= 0 && i < lines.length; i += step) {
      const raw = lines[i]!;
      if (!raw.trim() || raw.trimStart().startsWith("#")) continue;
      const l = readLine(raw);
      if (l.content < col) break;
      if (l.content === col && l.key) held.add(l.key);
      // An item of a list starts its map on its own line, and the next item starts another.
      if (l.dash && l.content === col) {
        if (step < 0) break;
        if (i !== at) break;
      }
    }
  };
  walk(-1);
  walk(1);
  return held;
}
