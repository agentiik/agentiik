import { refusal, type API } from "../api/client";
import type { components } from "../api/schema";

// One run as the inspector reads it, from GET /api/v1/runs/{id}, again while it has not ended.

export type RunDetail = components["schemas"]["runDetail"];
export type StepSummary = components["schemas"]["stepSummary"];
export type TaskSummary = components["schemas"]["taskSummary"];
export type EnvelopeReference = components["schemas"]["envelopeReference"];

const ended = new Set<RunDetail["state"]>(["succeeded", "failed", "cancelled", "timed_out"]);

export class RunReader {
  run = $state<RunDetail | null>(null);
  // missing is a run the API answered 404 for: absent, or not the caller's, which read alike.
  missing = $state(false);
  refused = $state("");
  readonly #api: API;
  readonly #id: string;

  constructor(api: API, id: string) {
    this.#api = api;
    this.#id = id;
  }

  get ended(): boolean {
    return this.run !== null && ended.has(this.run.state);
  }

  async read(): Promise<void> {
    const { data, error, response } = await this.#api.GET("/api/v1/runs/{id}", { params: { path: { id: this.#id } } });
    if (data) {
      this.run = data;
      this.missing = false;
      this.refused = "";
      return;
    }
    if (response.status === 404) {
      this.missing = true;
      return;
    }
    this.refused = refusal(response, error).message;
  }
}

// tasksOf is a step's tasks, the last attempt first and its shards in order, which is what a person
// reading a failure looks at.
export function tasksOf(run: RunDetail, step: string): TaskSummary[] {
  return run.tasks
    .filter((t) => t.step === step)
    .sort((a, b) => b.attempt - a.attempt || (a.shard?.index ?? 0) - (b.shard?.index ?? 0));
}

// lastAttempt is each shard's latest task of a step: what the step's strip of cells shows.
export function lastAttempt(run: RunDetail, step: string): TaskSummary[] {
  const byShard = new Map<number, TaskSummary>();
  for (const t of run.tasks) {
    if (t.step !== step) continue;
    const key = t.shard?.index ?? 0;
    const held = byShard.get(key);
    if (!held || t.attempt > held.attempt) byShard.set(key, t);
  }
  return [...byShard.values()].sort((a, b) => (a.shard?.index ?? 0) - (b.shard?.index ?? 0));
}
