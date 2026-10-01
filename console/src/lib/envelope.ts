import { refusal, Refusal, type API } from "../api/client";
import type { components } from "../api/schema";

// What a step published on a port, or what a task was handed on one, read under run:read_data: the
// envelope itself, which the API holds to its digest before a byte of it is answered.

// The envelope as envelope.schema.json describes it, { meta, items }. Written from its parts rather
// than taken whole, since the generated type also requires the schema's own $defs, which a document
// never carries, and types an item's data, any object, as one that may hold nothing.
export type Item = Omit<components["schemas"]["item"], "data"> & { data: Record<string, unknown> };
export type File = components["schemas"]["file"];
export type Envelope = { meta: components["schemas"]["meta"]; items: Item[] };

export type Side = "output" | "input";

// readOutput reads a workflow output of the run: the envelope its step published on the port the
// output is a view of, recorded once the run has ended with every output it declares published.
export async function readOutput(api: API, run: string, name: string): Promise<Envelope> {
  const answer = await api.GET("/api/v1/runs/{id}/outputs/{name}", { params: { path: { id: run, name } } });
  if (answer.data) {
    return answer.data;
  }
  return Promise.reject(purgedOr(refusal(answer.response, answer.error)));
}

// purgedOr is a refusal said for a person, the envelope purged with the run's retention said as such.
function purgedOr(r: Refusal): Refusal {
  return r.status === 410 ? new Refusal(410, `This envelope was purged with the run's retention: ${r.message}`) : r;
}

// readEnvelope reads one envelope: the one the step published on port, or, for the input side, the
// one the task was handed on port, by its attempt and its shard.
export async function readEnvelope(
  api: API,
  run: string,
  step: string,
  port: string,
  side: Side,
  task?: { attempt: number; shard?: { index: number } },
): Promise<Envelope> {
  const path = { id: run, step, port };
  const answer =
    side === "output"
      ? await api.GET("/api/v1/runs/{id}/steps/{step}/outputs/{port}", { params: { path } })
      : await api.GET("/api/v1/runs/{id}/steps/{step}/inputs/{port}", {
          params: { path, query: { attempt: task?.attempt, shard: task?.shard?.index } },
        });
  if (answer.data) {
    return answer.data;
  }
  // Purged with the run's retention: the run still shows its digest, and the bytes are gone.
  return Promise.reject(purgedOr(refusal(answer.response, answer.error)));
}

// shown is the envelope with its first count items, which is what the pane draws at once: an
// envelope is up to envelope_max_bytes and max_items, and every item drawn is a node of the page.
export function shown(e: Envelope, count: number): Envelope {
  return { meta: e.meta, items: e.items.slice(0, count) };
}

// filesOf is every file the items drawn carry, beside the item that carries it.
export function filesOf(items: Item[]): { item: string; file: File }[] {
  return items.flatMap((item) => (item.files ?? []).map((file) => ({ item: item.id, file })));
}

// A token of JSON as the pane colours it: a key, a string, a number, true, false or null, or the
// punctuation and white space between them.
export type Token = { kind: "key" | "string" | "number" | "literal" | "plain"; text: string };

// tokens is value written as JSON with two spaces of indent, cut into the tokens the pane colours.
// Written from the value rather than by matching the text, so that a string holding what looks
// like JSON is one string.
export function tokens(value: unknown): Token[] {
  const out: Token[] = [];
  const plain = (text: string) => {
    const last = out[out.length - 1];
    if (last && last.kind === "plain") last.text += text;
    else out.push({ kind: "plain", text });
  };
  const write = (v: unknown, indent: string) => {
    if (v === null || typeof v === "boolean") {
      out.push({ kind: "literal", text: String(v) });
    } else if (typeof v === "number") {
      out.push({ kind: "number", text: JSON.stringify(v) });
    } else if (typeof v === "string") {
      out.push({ kind: "string", text: JSON.stringify(v) });
    } else if (Array.isArray(v)) {
      if (v.length === 0) return plain("[]");
      plain("[\n");
      v.forEach((x, i) => {
        plain(indent + "  ");
        write(x, indent + "  ");
        plain(i < v.length - 1 ? ",\n" : "\n");
      });
      plain(indent + "]");
    } else if (typeof v === "object") {
      const entries = Object.entries(v as Record<string, unknown>).filter(([, x]) => x !== undefined);
      if (entries.length === 0) return plain("{}");
      plain("{\n");
      entries.forEach(([k, x], i) => {
        plain(indent + "  ");
        out.push({ kind: "key", text: JSON.stringify(k) });
        plain(": ");
        write(x, indent + "  ");
        plain(i < entries.length - 1 ? ",\n" : "\n");
      });
      plain(indent + "}");
    }
  };
  write(value, "");
  return out;
}
