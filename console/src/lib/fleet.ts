// What the runners view reads off GET /api/v1/runners and GET /api/v1/runner-pools: the condition
// of each runner, what each pool offers, and which namespaces may send work to it.

import type { Namespace } from "../api/client";
import type { components } from "../api/schema";
import { bytes } from "./stats";

export type Runner = components["schemas"]["runner"];
export type Pool = components["schemas"]["runnerPoolList"]["runner_pools"][number]["pool"];

// A runner heartbeats every 10 seconds, and three missed intervals move the tasks it held to lost:
// past that, what it last reported is no longer what it does, so it is said to be silent instead.
export const silentAfter = 30_000;

export type Condition = "ready" | "draining" | "revoked" | "unhealthy" | "silent";

// condition is one word for a runner, the installation's order first, since that is what is obeyed:
// revoked or draining whatever the runner says. Then whether it is heard from at all, and only then
// what it said of itself at its last heartbeat.
export function condition(r: Runner, now: number): Condition {
  if (r.state === "revoked" || r.state === "draining") return r.state;
  const seen = r.last_seen_at ? Date.parse(r.last_seen_at) : NaN;
  if (Number.isNaN(seen) || now - seen > silentAfter) return "silent";
  if (r.reported_state === "unhealthy" || r.reported_state === "draining") return r.reported_state;
  return "ready";
}

// offered is how many tasks a pool's runners take at once now: the concurrency of each one ready,
// which is what GET /api/v1/stats/pools calls its capacity. A draining, unhealthy or silent runner
// offers nothing new, whatever it would hold.
export function offered(runners: Runner[], now: number): number {
  return runners.reduce((n, r) => n + (condition(r, now) === "ready" ? (r.concurrency ?? 0) : 0), 0);
}

// counted says how many runners are in each condition, in the order a reader looks for trouble last.
export function counted(runners: Runner[], now: number): string {
  const order: Condition[] = ["ready", "draining", "unhealthy", "silent", "revoked"];
  const n = new Map<Condition, number>();
  for (const r of runners) n.set(condition(r, now), (n.get(condition(r, now)) ?? 0) + 1);
  return order.filter((c) => n.has(c)).map((c) => `${n.get(c)} ${c}`).join(" · ");
}

// reachedBy names the namespaces whose steps may be sent to a pool: those the pool accepts, every
// one where it lists none, that also allow it, by listing it in allowed_runner_pools or by listing
// nothing. Both sides have to agree before a task is placed, which is what this reads for a person
// writing either list.
export function reachedBy(pool: Pool, namespaces: Namespace[]): string[] {
  return namespaces
    .filter((ns) => pool.namespaces.length === 0 || pool.namespaces.includes(ns.name))
    .filter((ns) => {
      const allowed = ns.quotas?.allowed_runner_pools;
      return allowed === undefined || allowed.includes(pool.name);
    })
    .map((ns) => ns.name);
}

// ceilings are a pool's caps on one task, as a step writes them, or none.
export function ceilings(pool: Pool): string {
  const c = pool.resource_ceilings;
  const parts = [c.cpu && `cpu ${c.cpu}`, c.memory && `memory ${c.memory}`, c.pids && `pids ${c.pids}`].filter(Boolean);
  return parts.length ? parts.join(" · ") : "none";
}

// capacity is what a runner's host has, as the agent measured it at the join, a whole number of a
// unit written without its decimal, since a host's memory and disk usually are one.
export function capacity(r: Runner): string {
  const whole = (n: number) => bytes(n).replace(/\.0 /, " ");
  return `${r.cpu} vCPU · ${whole(r.memory_bytes)} · ${whole(r.disk_bytes)} disk · ${r.architecture}`;
}

// A label a pool grants and a runner claims, key=value, as wire.schema.json writes it.
export const labelPattern = /^[a-z0-9]+(?:[._-][a-z0-9]+)*=[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$/;

// labelsOf reads the labels written in one field, apart at commas or spaces, each once, in the order
// written.
export function labelsOf(text: string): string[] {
  return [...new Set(text.split(/[\s,]+/).filter((l) => l !== ""))];
}

// joinCommand is what an administrator runs on a host to make it a runner of the pool a join token
// was issued from, as root (#registering-a-runner): the API it joins, the token, the labels it
// claims where it claims any, and --replace for a host that has joined before, which joins as a new
// runner and leaves the old one registered until it is revoked.
export function joinCommand(api: string, token: string, labels: string[], replace = false): string {
  return ["agk-runner join", `--api ${api}`, `--token ${token}`, ...(labels.length ? [`--labels ${labels.join(",")}`] : []), ...(replace ? ["--replace"] : [])].join(" ");
}

// joinEnvironment is the same, for a runner its environment configures at every start, as a Compose
// file does: serve joins by itself with the token, and joins again as a new runner wherever the
// labels differ from those it joined with, so it needs no --replace.
export function joinEnvironment(api: string, token: string, labels: string[]): string {
  return [`AGK_API=${api}`, `AGK_RUNNER_JOIN_TOKEN=${token}`, ...(labels.length ? [`AGK_RUNNER_LABELS=${labels.join(",")}`] : [])].join("\n");
}
