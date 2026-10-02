// A workflow file resolved in the browser, for display only, as the engine resolves it for a run:
// its includes first, in declaration order, each applied once where resolution first reaches it;
// then the blocks each step extends, depth first, the outermost first; then defaults; then the
// step's own values. The editor draws the graph of the file being edited from this, since no
// version of it exists yet for the API to have resolved, and writes back nothing it resolved: an
// edit splices the entry point as it is written, and what an included file or a hidden block gives
// a step stays there.
//
// The keywords merge as the documentation's Includes and inheritance says: vars by name, defaults
// keyword by keyword, secrets each once in the order first named, params by name across every
// layer, before_script and after_script kept from every layer, defaults first, then each extended
// block from the outermost in, then the step's own; every other keyword a layer writes replaces it
// whole. A hidden block written again replaces the earlier one whole.

import { parse } from "yaml";
import type { Graph, Step } from "../graph";

// read answers a file of the tree at the version being edited, by its path from the root, or
// undefined where it is not read.
export type Read = (path: string) => string | undefined;

export type Resolved = {
  graph: Graph;
  // written says where each step is written: in the entry point, or in a file it includes.
  written: Map<string, "entry" | "included">;
  // problems are what could not be resolved, each said in a sentence, the graph drawn without it.
  problems: string[];
};

type Doc = Record<string, unknown>;

const isMap = (v: unknown): v is Doc => typeof v === "object" && v !== null && !Array.isArray(v);

// pathOf is where an include names a file, from the directory of the file naming it or from the
// root where it starts with /, cleaned; undefined where it leaves the tree.
export function pathOf(from: string, include: string): string | undefined {
  const dir = from.includes("/") ? from.slice(0, from.lastIndexOf("/")) : "";
  const parts = (include.startsWith("/") ? include.slice(1) : (dir ? dir + "/" : "") + include).split("/");
  const out: string[] = [];
  for (const p of parts) {
    if (p === "" || p === ".") continue;
    if (p === "..") {
      if (out.length === 0) return undefined;
      out.pop();
      continue;
    }
    out.push(p);
  }
  return out.length ? out.join("/") : undefined;
}

// includedPaths are the files of the tree an entry point's path includes read, its own path
// includes after each, as far as read answers them: what the editor fetches before it resolves.
export function includedPaths(entry: string, read: Read): string[] {
  const seen: string[] = [];
  const walk = (from: string, text: string | undefined) => {
    const doc = load(text);
    for (const inc of includesOf(doc)) {
      if (typeof inc.path !== "string") continue;
      const p = pathOf(from, inc.path);
      if (!p || seen.includes(p)) continue;
      seen.push(p);
      walk(p, read(p));
    }
  };
  walk("agentiik.yaml", entry);
  return seen;
}

function load(text: string | undefined): Doc {
  if (text === undefined) return {};
  try {
    const v = parse(text, { version: "1.2" });
    return isMap(v) ? v : {};
  } catch {
    return {};
  }
}

function includesOf(doc: Doc): Doc[] {
  return Array.isArray(doc.include) ? doc.include.filter(isMap) : [];
}

// The keywords merged otherwise than replaced.
const scripts = ["before_script", "after_script"];

// layer applies one layer of a step's values over those below it.
function layer(below: Doc, above: Doc): Doc {
  const out: Doc = { ...below };
  for (const [k, v] of Object.entries(above)) {
    if (k === "extends") continue;
    if (k === "params" && isMap(v) && isMap(out.params)) out.params = { ...out.params, ...v };
    else out[k] = v;
  }
  return out;
}

// resolve resolves an entry point, reading the files its path includes with read. base is the
// graph the version being edited resolved to on the API, from which the steps of what the browser
// cannot read, a workflow include of another repository, are drawn as they were.
export function resolve(entry: string, read: Read, base?: Graph): Resolved {
  const problems: string[] = [];
  const blocks = new Map<string, Doc>();
  const steps = new Map<string, Doc>();
  const written = new Map<string, "entry" | "included">();
  const declared: string[] = [];
  let defaults: Doc = {};
  let vars: Doc = {};
  const secrets: string[] = [];
  const applied = new Set<string>();
  const entered = new Set<string>();
  let unread = false;

  const apply = (path: string, doc: Doc, isEntry: boolean) => {
    entered.add(path);
    for (const inc of includesOf(doc)) {
      if (typeof inc.workflow === "string") {
        problems.push(`${inc.workflow} at ${String(inc.ref ?? "")} is included from another repository, whose steps are drawn as the version resolves them.`);
        unread = true;
        continue;
      }
      if (typeof inc.path !== "string") continue;
      const p = pathOf(path, inc.path);
      if (!p) {
        problems.push(`${inc.path}, which ${path} includes, leaves the tree.`);
        continue;
      }
      if (entered.has(p) && !applied.has(p)) {
        problems.push(`${p} includes itself, by way of ${path}.`);
        continue;
      }
      if (applied.has(p)) continue;
      const text = read(p);
      if (text === undefined) {
        problems.push(`${p}, which ${path} includes, could not be read; what it gives is drawn as the version resolves it.`);
        unread = true;
        continue;
      }
      apply(p, load(text), false);
    }
    // Hidden blocks at the root, and steps under steps named with a leading dot.
    for (const [k, v] of Object.entries(doc)) {
      if (k.startsWith(".") && isMap(v)) blocks.set(k, v);
    }
    if (isMap(doc.defaults)) defaults = { ...defaults, ...doc.defaults };
    if (isMap(doc.vars)) vars = { ...vars, ...doc.vars };
    if (Array.isArray(doc.secrets)) for (const s of doc.secrets) if (typeof s === "string" && !secrets.includes(s)) secrets.push(s);
    if (isMap(doc.steps)) {
      for (const [name, v] of Object.entries(doc.steps)) {
        if (!isMap(v)) continue;
        if (name.startsWith(".")) {
          blocks.set(name, v);
          continue;
        }
        // A step an included file writes is overridden by the entry point keyword by keyword.
        steps.set(name, steps.has(name) ? layer(steps.get(name)!, v) : v);
        if (!declared.includes(name)) declared.push(name);
        written.set(name, isEntry ? "entry" : (written.get(name) ?? "included"));
      }
    }
    applied.add(path);
  };

  const doc = load(entry);
  apply("agentiik.yaml", doc, true);

  // The blocks a step extends, depth first: the outermost, the one nothing above extends, first.
  const chain = (name: string, step: Doc): Doc[] => {
    const out: Doc[] = [];
    const seen = new Set<string>();
    let at = step.extends;
    while (typeof at === "string") {
      if (seen.has(at)) {
        problems.push(`${name} extends ${at}, which extends itself.`);
        break;
      }
      seen.add(at);
      const block = blocks.get(at);
      if (!block) {
        problems.push(`${name} extends ${at}, which no file writes.`);
        break;
      }
      out.unshift(block);
      at = block.extends;
    }
    return out;
  };

  const resolved: Record<string, Step> = {};
  for (const name of declared) {
    const own = steps.get(name)!;
    const ancestors = chain(name, own);
    let values: Doc = {};
    for (const b of ancestors) values = layer(values, b);
    values = layer(values, defaults);
    values = layer(values, own);
    for (const k of scripts) {
      const kept = [defaults, ...ancestors, own].flatMap((l) => (Array.isArray(l[k]) ? (l[k] as unknown[]) : []));
      if (kept.length) values[k] = kept;
    }
    resolved[name] = stepOf(values, ancestorsOf(own, blocks));
  }

  // What could not be read is drawn as the version resolved it: each step of base the browser did
  // not resolve and the entry point does not write.
  if (unread && base) {
    for (const name of base.order) {
      if (!resolved[name] && base.steps[name]) {
        resolved[name] = base.steps[name]!;
        declared.push(name);
        written.set(name, "included");
      }
    }
  }

  const graph: Graph = {
    ...(base ?? { workflow: "", commit: "", includes: [] }),
    inputs: isMap(doc.inputs) ? (doc.inputs as Graph["inputs"]) : base?.inputs,
    outputs: isMap(doc.outputs) ? (doc.outputs as Graph["outputs"]) : base?.outputs,
    vars: vars as Graph["vars"],
    secrets,
    order: orderOf(declared, resolved, problems),
    steps: resolved,
  };
  return { graph, written, problems };
}

// stepOf is a step's resolved values as the graph writes them: its kind, its edges in their long
// form, and the defaults the language gives left out.
function stepOf(v: Doc, extended: string[]): Step {
  const out: Doc = { ...v };
  delete out.extends;
  if (extended.length) out.extends = extended;
  out.kind = typeof v.workflow === "string" || isMap(v.workflow) ? "workflow" : v.script !== undefined ? "script" : "brick";
  if (typeof v.workflow === "string") {
    const [workflow, ref] = v.workflow.split("@");
    out.workflow = ref ? { workflow, ref } : { workflow };
  }
  if (Array.isArray(v.needs)) out.needs = v.needs.map(edgeOf).filter((e): e is NonNullable<ReturnType<typeof edgeOf>> => e !== undefined);
  if (v.merge === "wait_all") delete out.merge;
  if (isMap(v.strategy)) {
    const s = { ...v.strategy };
    if (s.fan_out === "none") delete s.fan_out;
    out.strategy = s;
  }
  return out as unknown as Step;
}

// ancestorsOf names the blocks a step extends, the one it names first, as the graph lists them.
function ancestorsOf(step: Doc, blocks: Map<string, Doc>): string[] {
  const out: string[] = [];
  let at = step.extends;
  while (typeof at === "string" && !out.includes(at)) {
    out.push(at);
    at = blocks.get(at)?.extends;
  }
  return out;
}

// edgeOf is an edge in its long form: the short form names the step alone, its out port onto in.
export function edgeOf(e: unknown): { step: string; port: string; as: string } | undefined {
  if (typeof e === "string") return { step: e, port: "out", as: "in" };
  if (isMap(e) && typeof e.step === "string") {
    return { step: e.step, port: typeof e.port === "string" ? e.port : "out", as: typeof e.as === "string" ? e.as : "in" };
  }
  return undefined;
}

// orderOf is the steps in an order their edges allow, each after every step it needs, and in the
// order they were declared where the edges leave it free; a cycle is said and its steps put last.
function orderOf(declared: string[], steps: Record<string, Step>, problems: string[]): string[] {
  const out: string[] = [];
  const done = new Set<string>();
  let left = declared.filter((n) => steps[n]);
  while (left.length) {
    const next = left.find((n) => (steps[n]!.needs ?? []).every((e) => done.has(e.step) || !steps[e.step]));
    if (!next) {
      problems.push(`${left.join(", ")} need one another, which no run can start.`);
      out.push(...left);
      break;
    }
    out.push(next);
    done.add(next);
    left = left.filter((n) => n !== next);
  }
  return out;
}
