import type { API } from "../api/client";
import type { Graph } from "./graph";

// The tool a workflow is, as a client of a collection holding it is told of it, built as the server
// builds it from the same version (api/collection_mcp.go): "one block, one tool, whose arguments are
// the workflow's inputs". Its inputSchema is an object of those inputs, each by its name and with its
// schema as it stands, required where the input is required and has no default, and no property
// beside them. Its outputSchema, where the block names an output carrying a schema and the tool
// waits for its run, is the envelope that output returns, {meta, items}, each item's data held to the
// output's schema, since the protocol holds a structured result to its outputSchema and the result
// is the envelope; an async tool, whose call answers the run and never the output, publishes none.
//
// Each schema is drawn as a client receives it: under draft 2020-12, every reference inside an
// input's or the output's schema rewritten into a pointer from the root, no $id, $anchor or embedded
// $schema left, and every object's keys in the order the server writes them. What the console
// cannot draw is what the server reads from the rest of the tree: each file a reference reaches,
// which the server embeds under $defs by its path and points the reference at, the console holds
// none of, so a reference into another file is drawn as the workflow writes it.

export type Tool = {
  name: string;
  title?: string;
  description: string;
  output?: string;
  mode: "sync" | "async";
  timeout?: string;
  annotations: Record<string, boolean>;
  inputSchema: Record<string, unknown>;
  outputSchema?: unknown;
};

type Block = {
  name?: string;
  title?: string;
  description: string;
  output?: string;
  mode?: "sync" | "async";
  timeout?: string;
  annotations?: Record<string, boolean>;
};

// toolOf is the tool a version's graph publishes, or null where its entry point declares no mcp
// block, or one written to the list of tools the language had before v0.7.0, which publishes none.
export function toolOf(graph: Graph, workflow: string): Tool | null {
  const block = graph.mcp as unknown as Block | undefined;
  if (!block || typeof block.description !== "string" || "tools" in block) return null;
  const inputs = (graph.inputs ?? {}) as Record<string, { schema?: unknown; required?: boolean; default?: unknown }>;
  const outputs = (graph.outputs ?? {}) as Record<string, { schema?: unknown }>;
  const names = Object.keys(inputs).sort();
  const properties: Record<string, unknown> = {};
  const required: string[] = [];
  for (const name of names) {
    const input = inputs[name]!;
    properties[name] = input.schema ?? true;
    if (input.required && input.default === undefined) required.push(name);
  }
  const input: Record<string, unknown> = { type: "object", properties, additionalProperties: false };
  if (required.length) input.required = required;
  const mode = block.mode ?? "sync";
  const schema = block.output ? outputs[block.output]?.schema : undefined;
  return {
    name: block.name ?? workflow,
    title: block.title,
    description: block.description,
    output: block.output,
    mode,
    timeout: block.timeout,
    annotations: block.annotations ?? {},
    // Each input's schema is a document the engine compiles on its own, as the output's is.
    inputSchema: bundled(input, names.map((n) => `/properties/${token(n)}`)),
    outputSchema: schema !== undefined && schema !== null && mode === "sync" ? bundled(envelope(schema), ["/properties/items/items/properties/data"]) : undefined,
  };
}

// published says whether the version a run naming no ref runs publishes a tool, which the MCP tab
// is drawn for on each of the workflow's screens, so that the tabs after it stay where they are from
// one screen to the next: read from the workflow, as its own page reads it.
export async function published(api: API, namespace: string, workflow: string): Promise<boolean> {
  const { data } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } } });
  return data?.graph ? toolOf(data.graph, workflow) !== null : false;
}

// envelope is what a sync call's structured result is, the output's envelope, its items' data held
// to the output's schema.
function envelope(data: unknown): Record<string, unknown> {
  return {
    type: "object",
    required: ["meta", "items"],
    properties: {
      meta: { type: "object" },
      items: { type: "array", items: { type: "object", required: ["id", "data", "files"], properties: { id: { type: "string" }, data, files: { type: "array" } } } },
    },
  };
}

// The base every schema a workflow writes is compiled under, so that a reference it makes resolves
// as the engine resolves it.
const treeBase = "agk://repo/";

// bundled is a schema as the server publishes it, but for the files of the tree, which the console
// does not hold: under draft 2020-12, each document in it, a place holding a schema the engine
// compiles on its own, with its references resolved against treeBase moved by every $id met on the
// way and rewritten into pointers from the root where they reach the document itself, and its $ids,
// anchors and embedded $schemas left out, since nothing resolves against them any more. A reference
// to another file, or one that cannot be placed, is kept as written.
function bundled(root: Record<string, unknown>, documents: string[]): Record<string, unknown> {
  const bundle = JSON.parse(JSON.stringify({ ...root, $schema: "https://json-schema.org/draft/2020-12/schema" })) as Record<string, unknown>;
  for (const at of documents) {
    const own = new Resources();
    own.name(treeBase, at);
    eachSchema(pointed(bundle, at), at, treeBase, (o, at, base) => {
      if (typeof o.$id === "string") own.name(base, at);
      for (const key of ["$anchor", "$dynamicAnchor"]) if (typeof o[key] === "string") own.anchor(base, o[key] as string, at);
    });
    eachSchema(pointed(bundle, at), at, treeBase, (o, _at, base) => {
      if (typeof o.$ref === "string") o.$ref = inBundle(bundle, o.$ref, base, own);
      delete o.$id;
      delete o.$anchor;
      delete o.$schema;
    });
  }
  return sorted(bundle) as Record<string, unknown>;
}

// Resources are the schemas of a document a reference resolves to by URL, each by its pointer in
// the bundle, and the anchors each declares.
class Resources {
  readonly at = new Map<string, string>();
  readonly anchors = new Map<string, Map<string, string>>();

  name(url: string, at: string): void {
    if (!this.at.has(url)) this.at.set(url, at);
  }

  anchor(url: string, anchor: string, at: string): void {
    if (!this.anchors.has(url)) this.anchors.set(url, new Map());
    this.anchors.get(url)!.set(anchor, at);
  }
}

// inBundle is a reference as the pointer to what it resolves to against base, where that is the
// document's own, or else as it is written.
function inBundle(bundle: unknown, ref: string, base: string, own: Resources): string {
  let u: URL;
  let fragment: string;
  try {
    u = new URL(ref, base);
    fragment = decodeURIComponent(u.hash.slice(1));
  } catch {
    return ref;
  }
  const key = resourceOf(u);
  let to = own.at.get(key);
  if (to === undefined) return ref;
  if (fragment.startsWith("/")) to += fragment;
  else if (fragment !== "") to = own.anchors.get(key)?.get(fragment);
  if (to === undefined || pointed(bundle, to) === undefined) return ref;
  return `#${escaped(to)}`;
}

// resourceOf is the URL a resource is named by, the URL without its fragment.
function resourceOf(u: URL): string {
  const v = new URL(u.href);
  v.hash = "";
  return v.href;
}

// escaped is a pointer written as a URL's fragment, as the server writes one: what a fragment may
// hold left as it is, and every other byte escaped.
function escaped(pointer: string): string {
  return [...new TextEncoder().encode(pointer)].map((b) => (b < 0x80 && /[A-Za-z0-9\-_.~$&+,/:;=?@!()*]/.test(String.fromCharCode(b)) ? String.fromCharCode(b) : `%${b.toString(16).toUpperCase().padStart(2, "0")}`)).join("");
}

// The keywords of JSON Schema 2020-12 whose value is a schema, an object of schemas or a list of
// them, with the ones earlier drafts wrote: what eachSchema walks into, so that a $ref, an $id or an
// $anchor is read where it is a keyword, and never under enum, const, default or examples.
const schemaKeywords = ["additionalProperties", "propertyNames", "items", "contains", "not", "if", "then", "else", "unevaluatedItems", "unevaluatedProperties", "contentSchema", "additionalItems"];
const schemasKeywords = ["properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"];
const listKeywords = ["allOf", "anyOf", "oneOf", "prefixItems", "items"];

type Visit = (o: Record<string, unknown>, at: string, base: string) => void;

// eachSchema visits every schema object of node, the pointer to it and the base its references
// resolve against: base, moved by the $id of every object from node down to it, its own included.
function eachSchema(node: unknown, at: string, base: string, visit: Visit): void {
  if (!isObject(node)) return;
  if (typeof node.$id === "string") {
    try {
      base = resourceOf(new URL(node.$id, base));
    } catch {
      return;
    }
  }
  visit(node, at, base);
  for (const key of schemaKeywords) if (!Array.isArray(node[key])) eachSchema(node[key], `${at}/${token(key)}`, base, visit);
  for (const key of schemasKeywords) {
    const named = node[key];
    if (isObject(named)) for (const name of Object.keys(named).sort()) eachSchema(named[name], `${at}/${token(key)}/${token(name)}`, base, visit);
  }
  for (const key of listKeywords) {
    const listed = node[key];
    if (Array.isArray(listed)) listed.forEach((v, i) => eachSchema(v, `${at}/${token(key)}/${i}`, base, visit));
  }
}

// pointed is what a JSON Pointer names in a document, and undefined where it names nothing.
function pointed(doc: unknown, pointer: string): unknown {
  if (pointer === "") return doc;
  for (const t of pointer.slice(1).split("/")) {
    const name = t.replaceAll("~1", "/").replaceAll("~0", "~");
    if (Array.isArray(doc)) {
      if (!/^(0|[1-9][0-9]*)$/.test(name) || Number(name) >= doc.length) return undefined;
      doc = doc[Number(name)];
    } else if (isObject(doc) && Object.hasOwn(doc, name)) {
      doc = doc[name];
    } else {
      return undefined;
    }
  }
  return doc;
}

// token is a name as one token of a JSON Pointer, as RFC 6901 escapes it.
function token(name: string): string {
  return name.replaceAll("~", "~0").replaceAll("/", "~1");
}

// sorted is a value with every object's keys in order, as the server's encoder writes a map.
function sorted(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sorted);
  if (!isObject(value)) return value;
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((k) => [k, sorted(value[k])]),
  );
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
