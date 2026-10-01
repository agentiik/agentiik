// The edits the graph makes to a workflow file: a step added or removed, an edge connected or
// taken away, a step's merge, fan_out or max_parallel changed. Each is a splice of the entry point
// as it is written, through the format-preserving tree, so that the file a review reads differs by
// the edit alone, and each is refused, with why, where the language would refuse what it writes:
// a name off its grammar or already taken, a port the step does not publish, an edge already
// there, a cycle, a step something still needs. A step an included file writes is edited there,
// never by an override the entry point would gain without anybody writing it.

import { isMap, isSeq } from "yaml";
import { Refused, scalarSource, type YamlTree } from "../yaml-tree";
import type { Graph } from "../graph";
import { edgeOf } from "./resolve";

const identifier = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;

export type Merge = "wait_all" | "zip" | "first";

// writtenHere refuses an edit of a step the entry point does not write.
function writtenHere(tree: YamlTree, step: string): void {
  const node = tree.nodeAt(["steps", step]);
  if (!node || !isMap(node)) throw new Refused(`${step} is defined in another file that agentiik.yaml includes. Edit it in that file`);
}

function named(what: string, name: string): void {
  if (!identifier.test(name) || name.length > 255) throw new Refused(`${what} ${JSON.stringify(name)} cannot be used: a name may hold only letters, digits, hyphens and underscores, must start with a letter or a digit, and is 255 characters at most`);
}

// addStep writes a step at the end of steps: its image and the ports it publishes.
export function addStep(tree: YamlTree, graph: Graph, name: string, image: string, outputs: string[]): YamlTree {
  named("The step", name);
  if (graph.steps[name] || tree.nodeAt(["steps", name])) throw new Refused(`There is already a step named ${name}. Choose another name`);
  if (image.trim() === "") throw new Refused("Give the image the step runs");
  if (outputs.length === 0) throw new Refused("Give the step at least one output port");
  for (const o of outputs) named("The port", o);
  if (new Set(outputs).size !== outputs.length) throw new Refused("Two output ports have the same name. Give each its own");
  const steps = tree.nodeAt(["steps"]);
  if (!steps || !isMap(steps)) throw new Refused("agentiik.yaml has no steps block to add the step to. Add one in the text first");
  return tree.insert(["steps"], name, { block: [`image: ${scalarSource(image.trim())}`, `outputs: [${outputs.map((o) => scalarSource(o)).join(", ")}]`] });
}

// removeStep takes a step out, once nothing needs it any more.
export function removeStep(tree: YamlTree, graph: Graph, name: string): YamlTree {
  writtenHere(tree, name);
  const needing = graph.order.filter((s) => (graph.steps[s]?.needs ?? []).some((e) => e.step === name));
  if (needing.length) throw new Refused(`${needing.join(", ")} ${needing.length === 1 ? "takes" : "take"} input from ${name}. Remove ${needing.length === 1 ? "that edge" : "those edges"} first`);
  const outputs = Object.entries(graph.outputs ?? {}).filter(([, o]) => o.from.step === name).map(([k]) => k);
  if (outputs.length) throw new Refused(`The workflow's output${outputs.length === 1 ? "" : "s"} ${outputs.join(", ")} ${outputs.length === 1 ? "comes" : "come"} from ${name}. Change the outputs block in the text first`);
  return tree.remove(["steps", name]);
}

// reaches says whether a step's edges lead, one after another, to another.
function reaches(graph: Graph, from: string, to: string): boolean {
  const seen = new Set<string>();
  const walk = (s: string): boolean => {
    if (s === to) return true;
    if (seen.has(s)) return false;
    seen.add(s);
    return (graph.steps[s]?.needs ?? []).some((e) => walk(e.step));
  };
  return walk(from);
}

// connect gives a step an edge from another's port, arriving on the input port named as: appended
// to its needs, which is written where it is not.
export function connect(tree: YamlTree, graph: Graph, from: { step: string; port: string }, to: { step: string; as: string }): YamlTree {
  writtenHere(tree, to.step);
  const source = graph.steps[from.step];
  if (!source) throw new Refused(`There is no step named ${from.step}`);
  if (!(source.outputs ?? ["out"]).includes(from.port)) throw new Refused(`${from.step} has no output port named ${from.port}`);
  named("The input port", to.as);
  if (from.step === to.step) throw new Refused("A step cannot be connected to itself");
  if (reaches(graph, from.step, to.step)) throw new Refused(`${from.step} already depends on ${to.step}, so this edge would make a loop, and a run could never start`);
  const target = graph.steps[to.step];
  if ((target?.needs ?? []).some((e) => e.step === from.step && e.port === from.port && e.as === to.as)) throw new Refused(`${to.step} already has that edge`);
  const edge = `{ step: ${scalarSource(from.step)}, port: ${scalarSource(from.port)}, as: ${scalarSource(to.as)} }`;
  const needs = tree.nodeAt(["steps", to.step, "needs"]);
  if (needs && isSeq(needs)) return tree.append(["steps", to.step, "needs"], edge);
  if (needs) throw new Refused(`The needs of ${to.step} are written in a form this editor cannot change. Edit them in the text`);
  return tree.insert(["steps", to.step], "needs", { block: [`- ${edge}`] });
}

// disconnect takes one of a step's edges away, by where it is in its needs; the last one takes
// needs with it, since the language holds a written needs to one edge at least.
export function disconnect(tree: YamlTree, step: string, index: number): YamlTree {
  writtenHere(tree, step);
  const needs = tree.nodeAt(["steps", step, "needs"]);
  if (!needs || !isSeq(needs) || !needs.items[index]) throw new Refused(`${step} has no such edge`);
  if (needs.items.length === 1) return tree.remove(["steps", step, "needs"]);
  return tree.removeItem(["steps", step, "needs"], index);
}

// edgesWritten are a step's edges as its entry point writes them, each in its long form, in order.
export function edgesWritten(tree: YamlTree, step: string): { step: string; port: string; as: string }[] {
  const needs = tree.nodeAt(["steps", step, "needs"]);
  if (!needs || !isSeq(needs)) return [];
  const value = tree.value() as { steps?: Record<string, { needs?: unknown[] }> };
  return (value.steps?.[step]?.needs ?? []).map(edgeOf).filter((e): e is NonNullable<ReturnType<typeof edgeOf>> => e !== undefined);
}

// setOrDrop writes one scalar of a step where it is written, and adds it where it is not, unless
// the value is the language's default, which a file that does not write it already says; null
// takes it out. What a file wrote stays written, so that an edit changes the line it changes alone.
function setOrDrop(tree: YamlTree, path: (string | number)[], value: string | number | null, unwritten?: string | number): YamlTree {
  const written = tree.nodeAt(path) !== undefined;
  if (value === null) return written ? tree.remove(path) : tree;
  if (written) return tree.set(path, value);
  if (value === unwritten) return tree;
  return tree.insert(path.slice(0, -1), String(path[path.length - 1]), { inline: scalarSource(value) });
}

// setMerge chooses how a step combines edges arriving on one port, wait_all being what a step that
// writes no merge does.
export function setMerge(tree: YamlTree, step: string, merge: Merge): YamlTree {
  writtenHere(tree, step);
  const node = tree.nodeAt(["steps", step, "merge"]);
  if (node && isMap(node)) throw new Refused(`${step} joins its inputs on a key, which can only be changed in the text`);
  return setOrDrop(tree, ["steps", step, "merge"], merge, "wait_all");
}

// setFanOut divides a step's work: none, the default, one container for the whole envelope; item,
// one per item; batch(n), one per n items. strategy is written where it is not, and taken out with
// its last key.
export function setFanOut(tree: YamlTree, step: string, fanOut: string): YamlTree {
  writtenHere(tree, step);
  if (!/^(none|item|batch\([1-9][0-9]*\))$/.test(fanOut)) throw new Refused(`fan_out must be none, item or batch(n), not ${fanOut}`);
  return setStrategy(tree, step, "fan_out", fanOut, "none");
}

// setMaxParallel bounds how many of a step's shards run at once, or lifts the bound.
export function setMaxParallel(tree: YamlTree, step: string, max: number | null): YamlTree {
  writtenHere(tree, step);
  if (max !== null && (!Number.isInteger(max) || max < 1)) throw new Refused("max_parallel must be a whole number, 1 or more");
  return setStrategy(tree, step, "max_parallel", max);
}

function setStrategy(tree: YamlTree, step: string, key: string, value: string | number | null, unwritten?: string): YamlTree {
  const strategy = tree.nodeAt(["steps", step, "strategy"]);
  if (strategy && !isMap(strategy)) throw new Refused(`The strategy of ${step} is written in a form this editor cannot change. Edit it in the text`);
  if (!strategy) {
    if (value === null || value === unwritten) return tree;
    return tree.insert(["steps", step], "strategy", { block: [`${key}: ${scalarSource(value)}`] });
  }
  // strategy holds one key at least, so that taking its last takes it too.
  const others = strategy.items.filter((p) => (p.key as { value?: unknown })?.value !== key).length;
  if (value === null && others === 0) return tree.remove(["steps", step, "strategy"]);
  return setOrDrop(tree, ["steps", step, "strategy", key], value, unwritten);
}
