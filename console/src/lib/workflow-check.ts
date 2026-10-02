import { Validator, type OutputUnit } from "@cfworker/json-schema";
import schema from "../../vendor/workflow.schema.json";
import { isSeq } from "yaml";
import type { Path, YamlTree } from "./yaml-tree";

// A workflow file held to workflow.schema.json in the browser, the schema the API holds it to,
// copied from agentiik/schemas beside the OpenAPI document: so that a file is refused on the line
// that is wrong before anything is sent, rather than by the push that carries it. The API checks it
// again in every case, and what it checks beyond the schema, a cycle or a port no image declares,
// is the API's to say.

export type Problem = { line: number; at: string; message: string };

let validator: Validator | undefined;

// The keywords that only say a part of the document below them failed, which the part says better.
const passing = new Set(["properties", "$ref", "$dynamicRef", "allOf", "items", "prefixItems", "unevaluatedProperties", "unevaluatedItems", "additionalProperties", "patternProperties", "dependentSchemas", "if", "then", "else", "contains", "propertyNames"]);

// check is what is wrong with a file: what it cannot be read as, or else where it breaks the schema,
// each on its line, in the order of the file.
export function check(tree: YamlTree): Problem[] {
  const unread = tree.problems;
  if (unread.length > 0) return unread.map((p) => ({ line: p.line, at: "", message: p.message }));
  validator ??= new Validator(schema as never, "2020-12", false);
  const result = validator.validate(tree.value());
  if (result.valid) return [];
  const leaves = result.errors.filter((e) => !passing.has(e.keyword));
  // A key the schema takes nowhere is a false schema's refusal, which the validator also gives a
  // key whose own value failed, and every map above a key that failed; it is kept only where nothing
  // more precise is said at its place or below it.
  const kept = leaves.filter((e) => e.keyword !== "false" || !leaves.some((o) => o !== e && (o.keyword !== "false" ? within(o.instanceLocation, e.instanceLocation) : o.instanceLocation.startsWith(`${e.instanceLocation}/`))));
  const out = new Map<string, Problem>();
  for (const e of kept) {
    const path = pathOf(tree, e.instanceLocation);
    const message = said(e, path);
    const key = `${e.instanceLocation} ${message}`;
    if (!out.has(key)) out.set(key, { line: tree.lineAt(path), at: path.join("."), message });
  }
  return [...out.values()].sort((a, b) => a.line - b.line);
}

// within says whether a location is the one given or below it.
function within(location: string, under: string): boolean {
  return location === under || location.startsWith(`${under}/`);
}

// pathOf is a JSON pointer as a path of the file, an index where the node it is in is a list.
function pathOf(tree: YamlTree, location: string): Path {
  const parts = location.replace(/^#\/?/, "").split("/").filter((p) => p !== "").map((p) => decodeURIComponent(p).replaceAll("~1", "/").replaceAll("~0", "~"));
  const path: Path = [];
  for (const p of parts) {
    const node = tree.nodeAt(path);
    path.push(node && isSeq(node) && /^\d+$/.test(p) ? Number(p) : p);
  }
  return path;
}

// said is an error as a person reads it: a key the schema takes nowhere named as such, the rest as
// the validator words it.
function said(e: OutputUnit, path: Path): string {
  if (e.keyword === "false") {
    const key = path[path.length - 1];
    const where = path.length > 1 ? path.slice(0, -1).join(".") : "the document";
    return `${String(key)} is not a key ${where} takes`;
  }
  return e.error;
}
