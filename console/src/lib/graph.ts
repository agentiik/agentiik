// A workflow's resolved graph laid out to be drawn, from what GET /api/v1/{ns}/workflows/{name}
// answers of its version: a node per step, its ports as declared, an edge per needs entry, and the
// workflow's inputs and outputs as chips at its boundary. Nothing is drawn that the graph does not
// hold: no stage, no file order, no edge a reader would infer.

import type { components } from "../api/schema";

export type Graph = components["schemas"]["resolvedGraph"];
export type Step = Graph["steps"][string];
export type Need = NonNullable<Step["needs"]>[number];

// Sizes in the drawing's own units, which the zoom scales: wide enough for a step's name, its
// image and a strip of shard cells; tall enough for three lines; and far enough apart for a port's
// name and an edge's item count between two rows.
export const node = { width: 280, height: 104, gapX: 72, gapY: 96 };
export const chip = { height: 26, gap: 64 };
// lane is how far beside the steps an edge passing a row runs, and pad the room kept for it on
// either side of the drawing.
const lane = 40;
const pad = 56;

export type Port = { name: string; x: number; y: number };
export type Placed = { step: string; x: number; y: number; layer: number; inputs: Port[]; outputs: Port[] };
export type Edge = { from: { step: string; port: string }; to: { step: string; port: string }; path: string; mid: { x: number; y: number }; kind: "data" | "rejected" | "error" };
export type Boundary = { name: string; x: number; y: number; width: number; to?: { step: string; port: string }; from?: { step: string; port: string }; path?: string };
export type Layout = { nodes: Placed[]; edges: Edge[]; inputs: Boundary[]; outputs: Boundary[]; width: number; height: number };

// inputPorts are a step's input ports: those its edges arrive on, in the order the file declares
// them, then those fed by a workflow input or an expression.
export function inputPorts(s: Step): string[] {
  return [...new Set([...(s.needs ?? []).map((n) => n.as), ...Object.keys(s.inputs ?? {})])];
}

// fedBy is the workflow input an input port's expression reads where it reads one and nothing else,
// which is what the graph draws as an edge from the boundary.
export function fedBy(expression: unknown): string | undefined {
  if (typeof expression !== "string") return undefined;
  const m = /^\s*\$\{\{\s*(?:workflow\.)?inputs\.([A-Za-z0-9_-]+)\s*\}\}\s*$/.exec(expression);
  return m?.[1];
}

// kindOf is how a port's edge is drawn: an error port in the failed colour, a rejected port in the
// waiting colour and dashed, every other port as data.
export function kindOf(port: string): Edge["kind"] {
  return port === "error" ? "error" : port === "rejected" ? "rejected" : "data";
}

// layers places each step one row below the lowest step it needs, the steps fed by nothing on the
// first row: the graph's own order, since a step can only run once what it needs has published.
export function layers(g: Graph): Map<string, number> {
  const out = new Map<string, number>();
  for (const step of g.order) {
    const s = g.steps[step];
    const below = (s?.needs ?? []).map((n) => out.get(n.step) ?? 0);
    out.set(step, below.length === 0 ? 0 : Math.max(...below) + 1);
  }
  return out;
}

const spread = (count: number, width: number) => Array.from({ length: count }, (_, i) => ((i + 1) * width) / (count + 1));

// layout places the graph top to bottom: the workflow's inputs as a row of chips above it, its steps
// by layer, each layer in the graph's order and then moved under the steps it needs where that
// crosses fewer edges, and its outputs as a row of chips below.
export function layout(g: Graph): Layout {
  const layer = layers(g);
  const rows: string[][] = [];
  for (const step of g.order) {
    const l = layer.get(step) ?? 0;
    (rows[l] ??= []).push(step);
  }

  // One sweep down: each row ordered by where the steps it needs sit, its graph order breaking ties.
  const column = new Map<string, number>();
  rows.forEach((row, l) => {
    if (l > 0) {
      const weight = (step: string) => {
        const above = (g.steps[step]?.needs ?? []).map((n) => column.get(n.step) ?? 0);
        return above.length ? above.reduce((a, b) => a + b, 0) / above.length : Number.MAX_SAFE_INTEGER;
      };
      row.sort((a, b) => weight(a) - weight(b) || g.order.indexOf(a) - g.order.indexOf(b));
    }
    row.forEach((step, i) => column.set(step, i));
  });

  const widest = Math.max(1, ...rows.map((r) => r.length));
  const inputNames = Object.keys(g.inputs ?? {});
  const outputNames = Object.keys(g.outputs ?? {});
  const top = inputNames.length ? chip.height + chip.gap : 0;
  const inner = Math.max(widest * node.width + (widest - 1) * node.gapX, inputNames.length * 140, outputNames.length * 140);
  const width = inner + pad * 2;

  const nodes: Placed[] = [];
  const at = new Map<string, Placed>();
  rows.forEach((row, l) => {
    const rowWidth = row.length * node.width + (row.length - 1) * node.gapX;
    const left = pad + (inner - rowWidth) / 2;
    row.forEach((step, i) => {
      const s = g.steps[step]!;
      const x = left + i * (node.width + node.gapX);
      const y = top + l * (node.height + node.gapY);
      const ins = inputPorts(s);
      const outs = s.outputs ?? [];
      const p: Placed = {
        step,
        x,
        y,
        layer: l,
        inputs: spread(ins.length, node.width).map((dx, k) => ({ name: ins[k]!, x: x + dx, y })),
        outputs: spread(outs.length, node.width).map((dx, k) => ({ name: outs[k]!, x: x + dx, y: y + node.height })),
      };
      nodes.push(p);
      at.set(step, p);
    });
  });

  const bottom = top + rows.length * (node.height + node.gapY) - node.gapY;
  const curve = (x1: number, y1: number, x2: number, y2: number) => {
    const bend = Math.max(24, (y2 - y1) / 2);
    return `M ${x1} ${y1} C ${x1} ${y1 + bend}, ${x2} ${y2 - bend}, ${x2} ${y2}`;
  };

  // An edge that passes a row of steps runs beside it rather than through it: down from its port,
  // across to a lane left or right of the steps it passes, whichever side its port is nearer, down
  // the lane, and across to the port it arrives on, each edge on a lane of its own so that two never
  // run as one. Rows are counted from the first step row, the inputs' chips above it being row -1
  // and the outputs' below the last.
  const used = { left: 0, right: 0 };
  const route = (x1: number, y1: number, x2: number, y2: number, fromRow: number, toRow: number): { path: string; mid: { x: number; y: number } } => {
    const passed = nodes.filter((n) => n.layer > fromRow && n.layer < toRow);
    if (passed.length === 0) return { path: curve(x1, y1, x2, y2), mid: { x: (x1 + x2) / 2, y: (y1 + y2) / 2 } };
    const lo = Math.min(...passed.map((n) => n.x));
    const hi = Math.max(...passed.map((n) => n.x + node.width));
    const side = x1 <= (lo + hi) / 2 ? "left" : "right";
    const x = side === "left" ? lo - lane - 14 * used.left++ : hi + lane + 14 * used.right++;
    const a = y1 + Math.min(node.gapY, chip.gap) / 2;
    const b = y2 - Math.min(node.gapY, chip.gap) / 2;
    const r = 10;
    const sx = Math.sign(x - x1) || 1;
    const tx = Math.sign(x2 - x) || 1;
    const path = [
      `M ${x1} ${y1}`,
      `V ${a - r}`,
      `Q ${x1} ${a} ${x1 + sx * r} ${a}`,
      `H ${x - sx * r}`,
      `Q ${x} ${a} ${x} ${a + r}`,
      `V ${b - r}`,
      `Q ${x} ${b} ${x + tx * r} ${b}`,
      `H ${x2 - tx * r}`,
      `Q ${x2} ${b} ${x2} ${b + r}`,
      `V ${y2}`,
    ].join(" ");
    return { path, mid: { x, y: (a + b) / 2 } };
  };

  const edges: Edge[] = [];
  for (const p of nodes) {
    for (const n of g.steps[p.step]?.needs ?? []) {
      const from = at.get(n.step)?.outputs.find((o) => o.name === n.port);
      const to = p.inputs.find((i) => i.name === n.as);
      if (!from || !to) continue;
      const r = route(from.x, from.y, to.x, to.y, layer.get(n.step) ?? 0, p.layer);
      edges.push({ from: { step: n.step, port: n.port }, to: { step: p.step, port: n.as }, path: r.path, mid: r.mid, kind: kindOf(n.port) });
    }
  }

  const chips = (names: string[], y: number) => {
    const w = 120;
    const total = names.length * w + (names.length - 1) * 20;
    const left = pad + (inner - total) / 2;
    return names.map((name, i) => ({ name, x: left + i * (w + 20), y, width: w }));
  };
  const inputs: Boundary[] = chips(inputNames, 0).map((c) => {
    const target = nodes.flatMap((p) => Object.entries(g.steps[p.step]?.inputs ?? {}).filter(([, e]) => fedBy(e) === c.name).map(([port]) => ({ step: p.step, port, layer: p.layer, at: p.inputs.find((i) => i.name === port)! })))[0];
    return target ? { ...c, to: { step: target.step, port: target.port }, path: route(c.x + c.width / 2, c.y + chip.height, target.at.x, target.at.y, -1, target.layer).path } : c;
  });
  const outY = bottom + chip.gap;
  const outputs: Boundary[] = chips(outputNames, outY).map((c) => {
    const from = g.outputs?.[c.name]?.from;
    const anchor = from ? at.get(from.step)?.outputs.find((o) => o.name === from.port) : undefined;
    return from && anchor ? { ...c, from, path: route(anchor.x, anchor.y, c.x + c.width / 2, c.y, at.get(from.step)!.layer, rows.length).path } : c;
  });

  return { nodes, edges, inputs, outputs, width, height: outputNames.length ? outY + chip.height : bottom };
}

// scheduling is what decides when a step runs and how, in the words the file writes them, for the
// line under its name: a fan-out and how many shards run at once, a merge on a port several edges
// arrive on, the states it starts on where they are not the default, its condition, whether a
// failure here leaves the run going, and its retries and cache.
export function scheduling(s: Step): string[] {
  const out: string[] = [];
  const st = s.strategy;
  if (st?.matrix) out.push(`matrix: ${Object.keys(st.matrix).join(" × ")}`);
  else if (st?.fan_out !== undefined) out.push(`fan_out: ${String(st.fan_out)}`);
  if (st?.max_parallel !== undefined) out.push(`max_parallel: ${st.max_parallel}`);
  if (s.merge !== undefined) out.push(`merge: ${String(s.merge)}`);
  if (s.when && !(s.when.length === 1 && s.when[0] === "succeeded")) out.push(`when: ${s.when.join(", ")}`);
  if (s.if) out.push(`if: ${s.if}`);
  if (s.continue_on_error !== undefined) out.push("continue_on_error");
  if (s.retry?.max) out.push(`retry ×${s.retry.max}`);
  if (s.cache !== undefined) out.push("cache");
  return out;
}

// what is the line naming what a step runs: a brick by its image and release, a script by its base
// image and its commands, a call by the workflow it calls and where.
export function what(s: Step): string {
  if (s.kind === "workflow" && s.workflow) return `calls ${s.workflow.workflow}${s.workflow.ref ? `@${s.workflow.ref}` : ""}`;
  const image = (s.image ?? "").split("@")[0]!.replace(/^ghcr\.io\//, "");
  if (s.kind === "script") return `${image} · script, ${(s.script ?? []).length} ${(s.script ?? []).length === 1 ? "command" : "commands"}`;
  return s.brick ? `${image} · ${s.brick.name} ${s.brick.version}` : image;
}

// The triggers a version declares, as workflow.schema.json describes them: the resolved graph
// carries the on block as the file writes it, which the generated types read as any object.
export type Schedule = { cron: string; timezone?: string; jitter?: string; catch_up?: unknown };
export type Webhook = { path: string; method?: string; auth?: unknown; response?: string };
export type Event = { type: string; source?: string; namespace?: string; filter?: string };

export function triggers(g: Graph): { schedule: Schedule[]; webhook: Webhook[]; event: Event[] } {
  const on = (g.on ?? {}) as unknown as { schedule?: Schedule[]; webhook?: Webhook[]; event?: Event[] };
  return { schedule: on.schedule ?? [], webhook: on.webhook ?? [], event: on.event ?? [] };
}

// authOf is how a webhook is proved, in the word the file writes: hmac where it writes none.
export function authOf(w: Webhook): string {
  if (typeof w.auth === "string") return w.auth;
  if (w.auth && typeof w.auth === "object") return Object.keys(w.auth)[0] ?? "hmac";
  return "hmac";
}
