// Read and write the Agentiik brick contract.
//
// The envelope a brick is given on each input port, the parameters and the run context, the
// secrets mounted for it, one envelope per output port, the artifacts it attaches, and the exit
// code that decides its step's verdict. Nothing here is required: the contract is files and an
// exit code (https://agentiik.github.io/docs#brick), and a brick reading them itself is as much a
// brick. What this saves is what is easy to get subtly wrong: the metadata the runner refuses an
// envelope for, the size rules it refuses one for with exit code 121, and an exit code that is
// the runner's rather than the brick's.
//
// The Go and Python helpers have the same surface, name for name, and the scenarios under
// conformance/ hold all three to the same behaviour. Node's standard library is all this imports.

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

// The paths the contract names. Constants and not configuration, because a brick whose paths came
// from somewhere would be a brick whose contract depends on how it was run.
export const IN_DIR = "/agk/in";
export const OUT_PORTS = "/agk/out/ports";
export const OUT_FILES = "/agk/out/files";
export const PARAMS_FILE = "/agk/params.json";
export const RUN_FILE = "/agk/run.json";
export const SECRETS_DIR = "/agk/secrets";

export interface File {
  name: string;
  uri: string;
  media_type: string;
  size: number;
  sha256: string;
}

export interface Item {
  id: string;
  data: Record<string, unknown>;
  files: File[];
}

export interface Meta {
  run_id: string;
  step: string;
  port: string;
  attempt: number;
  count: number;
  produced_at: string;
}

export interface Envelope {
  meta: Meta;
  items: Item[];
}

let root = "";
let stdin: () => string | undefined = () => (process.stdin.isTTY ? undefined : readFileSync(0, "utf8"));

// setRoot prefixes every path of the contract, and is never called in a container. It exists for
// a test on the machine a brick is built on, which should not need a container to be told where
// bytes go.
export function setRoot(dir: string): void {
  root = dir;
}

// setStdin replaces where read takes the envelope of the port in when no file was mounted.
export function setStdin(read: () => string | undefined): void {
  stdin = read;
}

function at(path: string): string {
  return root ? join(root, path) : path;
}

function checkName(name: string, what: string): void {
  if (!name || name === "." || name === ".." || /[\\/]/.test(name)) {
    throw new Error(`${what} ${JSON.stringify(name)} is not a name: one segment of a path, no separator, not . or ..`);
  }
}

// The size rules an envelope is held to when the runner collects it. A breach fails the step with
// exit code 121, charged to the brick, so emit keeps to them itself.
export interface SizeLimits {
  inlineMax: number;
  envelopeMax: number;
  maxItems: number;
}

// The documentation's defaults. The contract tells a brick no other, so a brick on an
// installation that tightened them takes them as parameters and sets limits.
export const DEFAULT_LIMITS: Readonly<SizeLimits> = Object.freeze({ inlineMax: 262144, envelopeMax: 4194304, maxItems: 100000 });

// The rules emit holds every envelope to. A field of zero is its default.
export const limits: SizeLimits = { ...DEFAULT_LIMITS };

function orDefault(l: SizeLimits): SizeLimits {
  return {
    inlineMax: l.inlineMax > 0 ? l.inlineMax : DEFAULT_LIMITS.inlineMax,
    envelopeMax: l.envelopeMax > 0 ? l.envelopeMax : DEFAULT_LIMITS.envelopeMax,
    maxItems: l.maxItems > 0 ? l.maxItems : DEFAULT_LIMITS.maxItems,
  };
}

// An envelope that breaks envelope_max_bytes or max_items with its heavy values spilled. Nothing
// more can be spilled, so it is the brick's to refuse: a step that meets it splits its batch
// upstream.
export class TooLarge extends Error {
  constructor(message: string) {
    super(`the envelope breaks the size rules: ${message}`);
    this.name = "TooLarge";
  }
}

interface Task {
  runId: string;
  step: string;
  attempt: number;
  ports: string[] | undefined;
}

// What the environment says about this container: the metadata every envelope has to carry,
// refused by the runner when it disagrees, and refused here by name when it is missing.
function task(): Task {
  const env = process.env;
  const missing = ["AGK_RUN_ID", "AGK_STEP", "AGK_ATTEMPT"].filter((v) => !env[v]);
  if (missing.length) {
    throw new Error(`${missing.join(", ")} not set: the runner sets them in every container, and a brick run by hand is given them the same way`);
  }
  const attempt = Number(env.AGK_ATTEMPT);
  if (!Number.isInteger(attempt) || attempt < 1) {
    throw new Error(`AGK_ATTEMPT is ${JSON.stringify(env.AGK_ATTEMPT)}, and an attempt is a whole number from 1`);
  }
  const ports = env.AGK_OUT_PORTS ? env.AGK_OUT_PORTS.split(",").filter((p) => p) : undefined;
  return { runId: env.AGK_RUN_ID!, step: env.AGK_STEP!, attempt, ports };
}

// encode is the JSON of a value as the engine writes it, Go's encoding/json: keys sorted, no
// space, <, > and & escaped, and the two line separators escaped. It is what a value is measured
// and spilled as.
export function encode(value: unknown): string {
  const sorted = (v: unknown): unknown => {
    if (Array.isArray(v)) return v.map(sorted);
    if (v && typeof v === "object") {
      return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sorted((v as Record<string, unknown>)[k])]));
    }
    return v;
  };
  return JSON.stringify(sorted(value))
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

// read is the envelope that arrived on an input port, from /agk/in/<port>/envelope.json.
//
// For the port in alone, a brick run by hand with no file mounted reads it from standard input,
// never from a terminal. A port nobody fed is an empty batch rather than an error: an edge above
// that published nothing is the ordinary case.
export function read(port: string): Envelope {
  checkName(port, "port");
  const path = at(`${IN_DIR}/${port}/envelope.json`);
  let text: string | undefined;
  if (existsSync(path)) text = readFileSync(path, "utf8");
  else if (port === "in") text = stdin();
  if (text === undefined || !text.trim()) {
    return { meta: { port } as Meta, items: [] };
  }
  let e: Envelope;
  try {
    e = JSON.parse(text);
  } catch (err) {
    throw new Error(`the envelope on port ${port} is not an envelope: ${(err as Error).message}`);
  }
  if (!e || typeof e !== "object") throw new Error(`the envelope on port ${port} is not an envelope: not an object`);
  e.items ??= [];
  for (const item of e.items) {
    item.data ??= {};
    item.files ??= [];
  }
  return e;
}

// items is the items of an input port, which is what most bricks want of read.
export function items(port: string): Item[] {
  return read(port).items;
}

function attachAs(t: Task, port: string, name: string, mediaType: string, content: Uint8Array): File {
  const dir = at(OUT_FILES);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, name), content);
  return {
    name,
    uri: `agk://run/${t.runId}/${t.step}/${port}/${name}`,
    media_type: mediaType,
    size: content.byteLength,
    sha256: createHash("sha256").update(content).digest("hex"),
  };
}

// attach writes an artifact under /agk/out/files/ and answers the files[] entry an item refers to
// it by, its size and digest computed here because the runner holds the item to them.
export function attach(port: string, name: string, mediaType: string, content: Uint8Array | string): File {
  checkName(name, "artifact");
  return attachAs(task(), port, name, mediaType, typeof content === "string" ? Buffer.from(content, "utf8") : content);
}

// segment files a string down to what one segment of a path and of a URI may hold, as the engine
// files the name of a value it spills.
export function segment(s: string): string {
  let out = "";
  for (const ch of s) {
    out += /^[A-Za-z0-9._-]$/.test(ch) ? ch : "_";
    if (out.length >= 64) break;
  }
  return out.replace(/\./g, "") === "" ? "field" : out;
}

function spilledName(id: string, rank: number, field: string): string {
  const base = `${segment(id || `item-${rank}`)}-${segment(field)}`;
  let name = `${base}.json`;
  for (let n = 2; existsSync(join(at(OUT_FILES), name)); n++) name = `${base}-${n}.json`;
  return name;
}

// emit writes the envelope of one output port at /agk/out/ports/<port>.json.
//
// The metadata comes from the environment and count from the items. The envelope is held to the
// size rules first: a value of data above inline_max_bytes is spilled to an artifact named
// <item>-<field>.json, the engine's own name for it, and an envelope still above
// envelope_max_bytes, or holding more than max_items, throws TooLarge rather than leaving the
// runner to refuse it with 121.
export function emit(port: string, published: Partial<Item>[] | undefined): void {
  checkName(port, "port");
  const t = task();
  if (t.ports && !t.ports.includes(port)) {
    throw new Error(`port ${port} is not one this step declares, which are ${t.ports.join(", ")}: the runner refuses an envelope on any other`);
  }
  const list = published ?? [];
  const l = orDefault(limits);
  if (list.length > l.maxItems) {
    throw new TooLarge(`port ${port} holds ${list.length} items and max_items is ${l.maxItems}`);
  }
  const out: Item[] = list.map((item, rank) => {
    const data = (item.data ??= {});
    const files = (item.files ??= []);
    for (const field of Object.keys(data).sort()) {
      const b = Buffer.from(encode(data[field]), "utf8");
      if (b.byteLength <= l.inlineMax) continue;
      files.push(attachAs(t, port, spilledName(item.id ?? "", rank, field), "application/json", b));
      delete data[field];
    }
    return { id: item.id ?? "", data, files };
  });
  const envelope: Envelope = {
    meta: { run_id: t.runId, step: t.step, port, attempt: t.attempt, count: out.length, produced_at: "1970-01-01T00:00:00Z" },
    items: out,
  };
  const body = Buffer.from(JSON.stringify(envelope), "utf8");
  if (body.byteLength > l.envelopeMax) {
    throw new TooLarge(`the envelope on port ${port} weighs ${body.byteLength} bytes with its heavy values spilled, and envelope_max_bytes is ${l.envelopeMax}`);
  }
  const dir = at(OUT_PORTS);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, `${port}.json`), Buffer.concat([body, Buffer.from("\n")]));
}

// params is /agk/params.json: what the step chose. A step that supplied nothing leaves the file
// absent, which is an empty object and not an error.
export function params(): Record<string, unknown> {
  const path = at(PARAMS_FILE);
  if (!existsSync(path)) return {};
  let p: unknown;
  try {
    p = JSON.parse(readFileSync(path, "utf8"));
  } catch (err) {
    throw new Error(`the parameters at ${PARAMS_FILE} are not a JSON object: ${(err as Error).message}`);
  }
  if (!p || typeof p !== "object" || Array.isArray(p)) throw new Error(`the parameters at ${PARAMS_FILE} are not a JSON object`);
  return p as Record<string, unknown>;
}

// param is one parameter, or undefined where the step did not supply it. An explicit zero is
// zero: a default applied over it is the bug a reader checks the manifest to rule out.
export function param<T = unknown>(name: string): T | undefined {
  const p = params();
  return Object.prototype.hasOwnProperty.call(p, name) ? (p[name] as T) : undefined;
}

// One shard of a fan-out: AGK_SHARD, 3/8 read as index 3 of count 8.
export interface Shard {
  index: number;
  count: number;
}

// The run context: /agk/run.json and the AGK_* variables. A field the runner did not give is
// left empty rather than invented.
export interface Run {
  id: string;
  workflow: string;
  version: string;
  namespace: string;
  commit: string;
  trigger_kind: string;
  triggered_by: string;
  started_at: string;
  step: string;
  attempt: number;
  shard: Shard | undefined;
  deadline: Date | undefined;
  repo: string;
  out_ports: string[];
  traceparent: string;
}

// context reads the run context: what the run is from /agk/run.json, what this container is from
// the environment.
export function context(): Run {
  const r: Run = {
    id: "", workflow: "", version: "", namespace: "", commit: "", trigger_kind: "", triggered_by: "", started_at: "",
    step: "", attempt: 0, shard: undefined, deadline: undefined, repo: "", out_ports: [], traceparent: "",
  };
  const path = at(RUN_FILE);
  if (existsSync(path)) {
    const doc = JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
    for (const key of ["id", "workflow", "namespace", "commit", "trigger_kind", "triggered_by", "started_at"] as const) {
      if (typeof doc[key] === "string") r[key] = doc[key] as string;
    }
  }
  const env = process.env;
  if (env.AGK_RUN_ID) r.id = env.AGK_RUN_ID;
  if (env.AGK_WORKFLOW) {
    const at = env.AGK_WORKFLOW.indexOf("@");
    r.workflow = at < 0 ? env.AGK_WORKFLOW : env.AGK_WORKFLOW.slice(0, at);
    r.version = at < 0 ? "" : env.AGK_WORKFLOW.slice(at + 1);
  }
  if (env.AGK_NAMESPACE) r.namespace = env.AGK_NAMESPACE;
  if (env.AGK_COMMIT) r.commit = env.AGK_COMMIT;
  r.step = env.AGK_STEP ?? "";
  if (env.AGK_ATTEMPT) {
    r.attempt = Number(env.AGK_ATTEMPT);
    if (!Number.isInteger(r.attempt)) throw new Error(`AGK_ATTEMPT is ${JSON.stringify(env.AGK_ATTEMPT)}, and an attempt is a whole number`);
  }
  if (env.AGK_SHARD) {
    const m = /^(\d+)\/(\d+)$/.exec(env.AGK_SHARD);
    if (!m) throw new Error(`AGK_SHARD is ${JSON.stringify(env.AGK_SHARD)}, and a shard is written index/count`);
    r.shard = { index: Number(m[1]), count: Number(m[2]) };
  }
  if (env.AGK_DEADLINE) {
    const d = new Date(env.AGK_DEADLINE);
    if (Number.isNaN(d.getTime())) throw new Error(`AGK_DEADLINE is ${JSON.stringify(env.AGK_DEADLINE)}, and a deadline is RFC 3339`);
    r.deadline = d;
  }
  r.repo = env.AGK_REPO ?? "";
  r.out_ports = (env.AGK_OUT_PORTS ?? "").split(",").filter((p) => p);
  r.traceparent = env.TRACEPARENT ?? "";
  return r;
}

// secret is the secret mounted at /agk/secrets/<name>, or undefined where none is: a secret the
// manifest declares optional may be absent. Never the environment, where every process the brick
// starts would inherit it and the first to dump its environment would print it.
export function secret(name: string): Buffer | undefined {
  checkName(name, "secret");
  const path = at(`${SECRETS_DIR}/${name}`);
  return existsSync(path) ? readFileSync(path) : undefined;
}

// An application failure, retried only where the step's retry.on says so.
export const FAILED = 1;
// A far side that may answer next time, retried by the step's policy.
export const TRANSIENT = 100;
// Input that will be as unusable on every attempt, never retried.
export const INVALID = 120;

// exitCode is the code exit leaves with: the code where a brick may leave by it, 1 to 120, and
// FAILED for anything else, which is the runner's or the runtime's.
export function exitCode(code: number): number {
  return Number.isInteger(code) && code >= FAILED && code <= INVALID ? code : FAILED;
}

// exit writes one line on standard error, the task's log, and leaves with code mapped by
// exitCode. Never standard output, which no log keeps.
export function exit(code: number, message: string): never {
  process.stderr.write(`${message}\n`);
  if (exitCode(code) !== code) {
    process.stderr.write(`exit code ${code} is not a brick's to leave with: 1 to 99 is a failure, 100 to 119 transient, 120 invalid input; leaving with ${exitCode(code)}\n`);
  }
  return process.exit(exitCode(code));
}

// fail leaves with FAILED: the step failed, and is retried only where it says so.
export function fail(message: string): never {
  return exit(FAILED, message);
}

// retry leaves with TRANSIENT: the far side was briefly unavailable. Never for a payload that will
// be as wrong next time.
export function retry(message: string): never {
  return exit(TRANSIENT, message);
}

// refuse leaves with INVALID: what the step or the batch says is unusable on every attempt.
export function refuse(message: string): never {
  return exit(INVALID, message);
}
