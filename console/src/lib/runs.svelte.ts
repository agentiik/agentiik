import { refusal, type API } from "../api/client";
import type { components } from "../api/schema";

// A namespace's runs as the runs view lists them, from GET /api/v1/runs: newest first, narrowed by a
// state, a workflow and how far back, a page at a time, and read again while the view is live.

export type Run = components["schemas"]["runList"]["runs"][number];
export type RunState = Run["state"];

export const states: readonly RunState[] = ["failed", "waiting", "running", "succeeded", "queued", "cancelled", "timed_out"];

// The spans a list reaches back over, each with its length, and every run the namespace keeps where
// none is chosen.
export const spans = {
  "1h": { label: "Last hour", ms: 3_600_000 },
  "24h": { label: "Last 24 hours", ms: 86_400_000 },
  "7d": { label: "Last 7 days", ms: 7 * 86_400_000 },
  "30d": { label: "Last 30 days", ms: 30 * 86_400_000 },
  all: { label: "Every run kept", ms: undefined },
} as const;

export type Span = keyof typeof spans;

// A list is narrowed by a span counted back from now, or by the bounds of a statistics bucket, which
// a point of a chart opens: since and until, both included, as GET /api/v1/runs takes them.
export type Filters = { state?: RunState; workflow?: string; span: Span; since?: string; until?: string };

// filtersOf reads the filters out of the address's query, ignoring what is no filter of this view.
export function filtersOf(query: URLSearchParams): Filters {
  const state = query.get("state") as RunState | null;
  const span = query.get("span") as Span | null;
  const workflow = query.get("workflow");
  const since = query.get("since");
  const until = query.get("until");
  const bounded = since !== null && until !== null && !Number.isNaN(Date.parse(since)) && !Number.isNaN(Date.parse(until));
  return {
    state: state && states.includes(state) ? state : undefined,
    workflow: workflow || undefined,
    span: span && span in spans ? span : "24h",
    since: bounded ? since : undefined,
    until: bounded ? until : undefined,
  };
}

// queryOf writes the filters back into a query, leaving out what is the default.
export function queryOf(filters: Filters): URLSearchParams {
  const q = new URLSearchParams();
  if (filters.state) q.set("state", filters.state);
  if (filters.workflow) q.set("workflow", filters.workflow);
  if (filters.since && filters.until) {
    q.set("since", filters.since);
    q.set("until", filters.until);
  } else if (filters.span !== "24h") {
    q.set("span", filters.span);
  }
  return q;
}

// A first page, then pages of 25 as the reader asks for them, as the documentation writes it: "page
// it with Load 25 more".
export const firstPage = 50;
export const nextPage = 25;

export class RunList {
  runs = $state<Run[]>([]);
  reading = $state(false);
  // exhausted is whether the last page read held fewer runs than asked for, so that no more is kept.
  exhausted = $state(false);
  refused = $state("");
  // settled is whether a first read has answered, so that what counts the runs is drawn once there is
  // something to count, rather than saying none and then moving down under the rows as they come.
  settled = $state(false);

  readonly #api: API;
  readonly #namespace: string;
  readonly #filters: Filters;
  readonly #now: () => number;

  constructor(api: API, namespace: string, filters: Filters, now: () => number = Date.now) {
    this.#api = api;
    this.#namespace = namespace;
    this.#filters = filters;
    this.#now = now;
  }

  #query(limit: number, until?: string) {
    const span = spans[this.#filters.span].ms;
    const since = this.#filters.since ?? (span === undefined ? undefined : new Date(this.#now() - span).toISOString());
    return {
      namespace: this.#namespace,
      workflow: this.#filters.workflow,
      state: this.#filters.state,
      since,
      until: until ?? this.#filters.until,
      limit,
    };
  }

  async #page(limit: number, until?: string): Promise<Run[] | undefined> {
    const { data, error, response } = await this.#api.GET("/api/v1/runs", { params: { query: this.#query(limit, until) } });
    if (!data) {
      this.refused = refusal(response, error).message;
      return undefined;
    }
    this.refused = "";
    return data.runs;
  }

  // read reads the first page, or again as many as are shown, so that a live list keeps its length.
  async read(): Promise<void> {
    this.reading = true;
    try {
      const limit = Math.min(500, Math.max(firstPage, this.runs.length));
      const runs = await this.#page(limit);
      if (runs) {
        this.runs = runs;
        this.exhausted = runs.length < limit;
      }
    } finally {
      this.reading = false;
      this.settled = true;
    }
  }

  // more reads the next page: the runs created at or before the oldest shown, since until is
  // included, less the ones already shown at that very instant.
  async more(): Promise<void> {
    const oldest = this.runs.at(-1);
    if (!oldest || this.exhausted) {
      return;
    }
    this.reading = true;
    try {
      const runs = await this.#page(nextPage + 1, oldest.created_at);
      if (runs) {
        const shown = new Set(this.runs.map((r) => r.run));
        const fresh = runs.filter((r) => !shown.has(r.run));
        this.runs = [...this.runs, ...fresh];
        this.exhausted = runs.length < nextPage + 1;
      }
    } finally {
      this.reading = false;
    }
  }
}
