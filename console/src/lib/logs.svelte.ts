import { refusal, type API } from "../api/client";

// A step's log as GET /api/v1/runs/{id}/steps/{step}/logs streams it: server-sent events, history
// then live, each dispatch whole and one after the other, secrets masked before anything was
// written. The browser's EventSource reads it, since it names the last event it was given when it
// reconnects, which is how the stream resumes where it was cut; and the console keeps each dispatch
// apart, since a task handed out again is one more dispatch of the same task.
//
// A dispatch is known by the task it runs, run/step/attempt and the shard after them, which is the
// idempotency key its dispatch event names and the task the run's record lists: that is how the
// inspector shows the log of the task chosen.

export type Line = { line: number; at: string; text: string };
export type Gap = { first: number; lines: number; reason: string };
export type Dispatch = {
  id: string;
  task: string;
  attempt: number;
  shard?: { index: number; of: number };
  requeue: number;
  lines: Line[];
  gaps: Gap[];
  // over is how the stream let go of the dispatch: the lines the API holds of it, whether the runner
  // cut it at its caps, and whether it is final or the dispatch was still running.
  over?: { lines: number; truncated: boolean; final: boolean };
};

// A source of events, as EventSource is one: the tests stand one in.
export interface Source {
  onerror: ((this: Source, ev: Event) => unknown) | null;
  onopen: ((this: Source, ev: Event) => unknown) | null;
  addEventListener(type: string, listener: (e: MessageEvent<string>) => void): void;
  close(): void;
  readonly readyState: number;
}
export type Sources = (url: string) => Source;

// sources is where streams come from: the browser's EventSource, which carries the session cookie
// to its own origin, or what a test stands in with streamsFrom.
let sources: Sources = (url) => new EventSource(url) as unknown as Source;

export function streamsFrom(s: Sources): void {
  sources = s;
}

// CLOSED is EventSource.CLOSED, which a source is in once it has given up, as it does on an answer
// other than 200: it reconnects by itself after a network failure, and never after a refusal.
const CLOSED = 2;

export class LogTail {
  dispatches = $state<Dispatch[]>([]);
  // verdict is the step's once the stream has said its log is over; the stream is closed then.
  verdict = $state<string | null>(null);
  refused = $state("");
  reconnecting = $state(false);

  readonly #api: API;
  readonly #run: string;
  readonly #step: string;
  #source: Source | null = null;

  constructor(api: API, run: string, step: string) {
    this.#api = api;
    this.#run = run;
    this.#step = step;
  }

  // open follows the stream at url, the route's address resolved against the page's <base>.
  open(url: string): void {
    const source = sources(url);
    this.#source = source;
    source.onopen = () => {
      this.reconnecting = false;
    };
    source.onerror = () => {
      if (source.readyState === CLOSED) {
        void this.#why();
        return;
      }
      this.reconnecting = this.verdict === null;
    };
    source.addEventListener("dispatch", (e) => {
      const d = JSON.parse(e.data) as { task_id: string; idempotency_key: string; attempt: number; shard?: { index: number; of: number }; requeue: number };
      // A reconnect that resumes in the middle of a dispatch is sent its dispatch event again.
      if (this.dispatches.some((x) => x.id === d.task_id)) return;
      this.dispatches.push({ id: d.task_id, task: d.idempotency_key, attempt: d.attempt, shard: d.shard, requeue: d.requeue, lines: [], gaps: [] });
    });
    source.addEventListener("line", (e) => {
      const l = JSON.parse(e.data) as { task_id: string; line: number; at: string; text: string };
      const d = this.#dispatch(l.task_id);
      if (d && (d.lines.length === 0 || d.lines[d.lines.length - 1]!.line < l.line)) {
        d.lines.push({ line: l.line, at: l.at, text: l.text });
      }
    });
    source.addEventListener("gap", (e) => {
      const g = JSON.parse(e.data) as { task_id: string; first_line: number; lines: number; reason: string };
      this.#dispatch(g.task_id)?.gaps.push({ first: g.first_line, lines: g.lines, reason: g.reason });
    });
    source.addEventListener("dispatch_end", (e) => {
      const o = JSON.parse(e.data) as { task_id: string; lines: number; truncated: boolean; final: boolean };
      const d = this.#dispatch(o.task_id);
      if (d) d.over = { lines: o.lines, truncated: o.truncated, final: o.final };
    });
    source.addEventListener("end", (e) => {
      this.verdict = (JSON.parse(e.data) as { verdict: string }).verdict;
      this.reconnecting = false;
      this.close();
    });
  }

  close(): void {
    this.#source?.close();
    this.#source = null;
  }

  // of is the dispatches of one task, in the order they were made.
  of(task: string): Dispatch[] {
    return this.dispatches.filter((d) => d.task === task);
  }

  #dispatch(id: string): Dispatch | undefined {
    return this.dispatches.find((d) => d.id === id);
  }

  // why asks the route again where the stream gave up, since an EventSource is told nothing of a
  // refusal: 410 once the logs went with the run's retention, 503 with no object store, 404 for a
  // step the run lacks. The answer's body is not read where it is the stream after all.
  async #why(): Promise<void> {
    const { response, error } = await this.#api.GET("/api/v1/runs/{id}/steps/{step}/logs", {
      params: { path: { id: this.#run, step: this.#step } },
      parseAs: "stream",
    });
    if (response.ok) {
      await response.body?.cancel();
      this.refused = "The log stream was cut, and the browser gave up on it.";
      return;
    }
    this.refused = refusal(response, error).message;
  }
}
