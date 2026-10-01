// The manual run form, field by field from the workflow inputs a version declares: each value read
// as its JSON Schema types it and held to that schema in the browser before the run is asked for,
// so that a value the API would refuse is refused where it was typed. The API binds the inputs
// again all the same, as agk run --local does: what the browser checks is a courtesy, never the
// rule.

import { Validator, type Schema as JsonSchema } from "@cfworker/json-schema";

export type Declared = { schema: unknown; required?: boolean; default?: unknown };

// How a value is typed in: a scalar its schema types as one in a field of its kind, a choice among
// an enum's values in a list, and anything else as JSON.
export type Kind = "string" | "number" | "integer" | "boolean" | "enum" | "json";
export type Field = { name: string; kind: Kind; required: boolean; hasDefault: boolean; default?: unknown; description?: string; options?: unknown[] };

export function fieldOf(name: string, d: Declared): Field {
  const schema = typeof d.schema === "object" && d.schema !== null ? (d.schema as Record<string, unknown>) : {};
  const base = { name, required: d.required === true, hasDefault: d.default !== undefined, default: d.default, description: typeof schema.description === "string" ? schema.description : undefined };
  if (Array.isArray(schema.enum) && schema.enum.every((v) => v === null || ["string", "number", "boolean"].includes(typeof v))) {
    return { ...base, kind: "enum", options: schema.enum };
  }
  const type = schema.type;
  if (type === "string" || type === "number" || type === "integer" || type === "boolean") return { ...base, kind: type };
  return { ...base, kind: "json" };
}

// The raw value a field starts with: its default, written the way the field takes it.
export function initial(f: Field): string | boolean {
  if (f.kind === "boolean") return f.default === true;
  if (f.default === undefined) return "";
  if (f.kind === "json") return JSON.stringify(f.default, null, 2);
  if (f.kind === "enum") return String(f.options?.findIndex((o) => o === f.default) ?? "");
  return String(f.default);
}

// Read is a field's value as the run would be given it, nothing where the field was left empty,
// which the API fills with the default, or why it cannot be read.
export type Read = { value?: unknown; empty: boolean; error?: string };

export function read(f: Field, raw: string | boolean): Read {
  if (f.kind === "boolean") return { value: raw === true, empty: false };
  const text = String(raw).trim();
  if (text === "") {
    return f.required && !f.hasDefault ? { empty: true, error: "required, and it declares no default" } : { empty: true };
  }
  switch (f.kind) {
    case "string":
      return { value: String(raw), empty: false };
    case "number":
    case "integer": {
      const n = Number(text);
      if (!Number.isFinite(n)) return { empty: false, error: "not a number" };
      if (f.kind === "integer" && !Number.isInteger(n)) return { empty: false, error: "not a whole number" };
      return { value: n, empty: false };
    }
    case "enum": {
      const i = Number(text);
      return Number.isInteger(i) && f.options && i >= 0 && i < f.options.length ? { value: f.options[i], empty: false } : { empty: false, error: "not one of the values it allows" };
    }
    default:
      try {
        return { value: JSON.parse(text), empty: false };
      } catch (e) {
        return { empty: false, error: `not JSON: ${e instanceof Error ? e.message : String(e)}` };
      }
  }
}

// The schemas are compiled as if the workflow file were at this address, so that a $ref naming a
// file of the tree, ./schemas/order.json, resolves to a path of the tree.
const root = "https://tree.invalid/agentiik.yaml";
const most = 16;

// refs are the files a schema names in its $ref keywords, as addresses resolved against base.
function refs(schema: unknown, base: string, into: Set<string>) {
  if (Array.isArray(schema)) {
    for (const s of schema) refs(s, base, into);
    return;
  }
  if (schema === null || typeof schema !== "object") return;
  for (const [k, v] of Object.entries(schema)) {
    if (k === "$ref" && typeof v === "string" && !v.startsWith("#")) {
      into.add(new URL(v.split("#")[0]!, base).href);
    } else {
      refs(v, base, into);
    }
  }
}

// validator compiles an input's schema with the files of the tree it names, read through file, a
// path of the tree to its JSON. Nothing where a file cannot be read, or more files are named than a
// form reads: the value is then the API's alone to check.
export async function validator(given: unknown, file: (path: string) => Promise<unknown>): Promise<Validator | undefined> {
  // Compiled from a copy: the validator marks the objects it compiles, which a page's own state
  // refuses, and a schema is JSON, which copies whole.
  const schema = given === undefined ? undefined : JSON.parse(JSON.stringify(given));
  if (typeof schema === "boolean") return new Validator(schema, "2020-12", false);
  if (schema === null || typeof schema !== "object") return undefined;
  const docs = new Map<string, unknown>();
  const wanted = new Set<string>();
  refs(schema, root, wanted);
  const queue = [...wanted];
  while (queue.length > 0) {
    const at = queue.shift()!;
    if (docs.has(at)) continue;
    if (docs.size >= most) return undefined;
    const found = await file(new URL(at).pathname.slice(1)).catch(() => undefined);
    const doc = found === undefined ? undefined : JSON.parse(JSON.stringify(found));
    if (doc === undefined) return undefined;
    docs.set(at, doc);
    const more = new Set<string>();
    refs(doc, at, more);
    for (const m of more) if (!docs.has(m)) queue.push(m);
  }
  const v = new Validator({ ...(schema as Record<string, unknown>), $id: root } as JsonSchema, "2020-12", false);
  for (const [at, doc] of docs) v.addSchema(doc as JsonSchema, at);
  return v;
}

// problems are why a value fails its schema, the most precise first: the keywords that only say a
// part of the value failed are left out where a deeper one says how.
export function problems(v: Validator, value: unknown): string[] {
  const r = v.validate(value);
  if (r.valid) return [];
  const wrapping = new Set(["properties", "items", "prefixItems", "allOf", "$ref", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "dependentSchemas", "if", "then", "else"]);
  const precise = r.errors.filter((e) => !wrapping.has(e.keyword));
  return (precise.length ? precise : r.errors).map((e) => {
    const at = e.instanceLocation.replace(/^#/, "");
    return at ? `at ${at}: ${e.error}` : e.error;
  });
}
