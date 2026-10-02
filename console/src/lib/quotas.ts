// A namespace's quotas as the administrator's form reads and writes them, and as a line reads them.

import type { components } from "../api/schema";
import { bytes } from "./stats";

export type Quotas = components["schemas"]["namespaceList"]["namespaces"][number]["quotas"] & {};

export const units = [
  { unit: "MiB", size: 1024 ** 2 },
  { unit: "GiB", size: 1024 ** 3 },
  { unit: "TiB", size: 1024 ** 4 },
] as const;
export type Unit = (typeof units)[number]["unit"];

// inUnits is a number of bytes in the largest unit that holds it whole, which is how a quota written
// in the form is read back; one no unit holds whole is in MiB, rounded up so it never reads as less.
export function inUnits(n: number): { amount: number; unit: Unit } {
  for (const u of [...units].reverse()) {
    if (n % u.size === 0) return { amount: n / u.size, unit: u.unit };
  }
  return { amount: Math.ceil(n / units[0].size), unit: "MiB" };
}

export function bytesOf(amount: number, unit: Unit): number {
  return amount * units.find((u) => u.unit === unit)!.size;
}

// Form is what the form holds: every field as typed, an empty one being a bound left out.
export type Form = {
  max_concurrent_tasks: string;
  max_retention_days: string;
  max_runs_per_hour: string;
  max_artifact: string;
  max_artifact_unit: Unit;
  max_run_duration: string;
  allowed_runner_pools: string[];
};

export function formOf(q: Quotas | undefined): Form {
  const artifact = q?.max_artifact_bytes !== undefined ? inUnits(q.max_artifact_bytes) : undefined;
  return {
    max_concurrent_tasks: String(q?.max_concurrent_tasks ?? 20),
    max_retention_days: q?.max_retention_days !== undefined ? String(q.max_retention_days) : "",
    max_runs_per_hour: q?.max_runs_per_hour !== undefined ? String(q.max_runs_per_hour) : "",
    max_artifact: artifact ? String(artifact.amount) : "",
    max_artifact_unit: artifact?.unit ?? "GiB",
    max_run_duration: q?.max_run_duration ?? "",
    allowed_runner_pools: [...(q?.allowed_runner_pools ?? [])],
  };
}

const whole = /^[1-9][0-9]*$/;
const duration = /^[1-9][0-9]*(ms|s|m|h|d)$/;

// bodyOf is what PUT /api/v1/namespaces/{ns}/quotas is sent, or the first field refused and why.
// A bound left empty is left out, which is the API's way of saying it bounds nothing, except the one
// that always holds a value; no pool ticked is no list, every pool that accepts the namespace.
export function bodyOf(f: Form): { body: Quotas } | { field: keyof Form; problem: string } {
  if (!whole.test(f.max_concurrent_tasks.trim())) return { field: "max_concurrent_tasks", problem: "a whole number from 1, which it always holds" };
  for (const k of ["max_retention_days", "max_runs_per_hour", "max_artifact"] as const) {
    if (f[k].trim() !== "" && !whole.test(f[k].trim())) return { field: k, problem: "a whole number from 1, or empty for no bound" };
  }
  if (f.max_run_duration.trim() !== "" && !duration.test(f.max_run_duration.trim())) {
    return { field: "max_run_duration", problem: "a length as a step's timeout writes it, such as 24h or 90m, or empty for no bound" };
  }
  const body: Quotas = { max_concurrent_tasks: Number(f.max_concurrent_tasks.trim()) };
  if (f.max_retention_days.trim()) body.max_retention_days = Number(f.max_retention_days.trim());
  if (f.max_runs_per_hour.trim()) body.max_runs_per_hour = Number(f.max_runs_per_hour.trim());
  if (f.max_artifact.trim()) body.max_artifact_bytes = bytesOf(Number(f.max_artifact.trim()), f.max_artifact_unit);
  if (f.max_run_duration.trim()) body.max_run_duration = f.max_run_duration.trim();
  if (f.allowed_runner_pools.length) body.allowed_runner_pools = [...f.allowed_runner_pools].sort();
  return { body };
}

// summary is a namespace's quotas on one line, what bounds nothing left out.
export function summary(q: Quotas | undefined): string {
  if (!q) return "";
  const parts = [`${q.max_concurrent_tasks ?? 20} tasks at once`];
  if (q.max_runs_per_hour !== undefined) parts.push(`${q.max_runs_per_hour} runs an hour`);
  if (q.max_artifact_bytes !== undefined) parts.push(`${bytes(q.max_artifact_bytes).replace(/\.0 /, " ")} of artifacts`);
  if (q.max_retention_days !== undefined) parts.push(`kept ${q.max_retention_days} days`);
  if (q.max_run_duration !== undefined) parts.push(`runs ${q.max_run_duration} at most`);
  parts.push(q.allowed_runner_pools ? `pools ${q.allowed_runner_pools.join(", ")}` : "every pool accepting it");
  return parts.join(" · ");
}
