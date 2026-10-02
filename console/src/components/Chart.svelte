<script lang="ts" module>
  // A tone is a colour of the design system's palette: a series that counts a state is drawn in that
  // state's colour, and every other series in the accent, in strengths of the one hue.
  export type Tone = "succeeded" | "failed" | "running" | "waiting" | "quiet" | "accent" | "accent-2" | "accent-3";

  export type Series = {
    label: string;
    tone: Tone;
    kind: "bars" | "line" | "area" | "step";
    values: (number | null)[];
    // dashed draws the span before the range, behind the range's own series.
    dashed?: boolean;
  };

  import { SvelteMap } from "svelte/reactivity";

  // The width each chart drawn at once needs for its values' axis, its widest label and the gap
  // beside it. Every chart takes the widest of them, so that charts one above the other start their
  // plots on one line, and none keeps room its labels do not fill: a fixed 64px left a chart of
  // single figures with 50px of nothing at its left.
  const needs = new SvelteMap<symbol, number>();
</script>

<script lang="ts">
  import { onMount, untrack } from "svelte";
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";

  // One chart of a statistics page, drawn with uPlot on a canvas. Every chart of a page shares its
  // range, its readout and its zoom: dragging across one narrows the range of all of them, a double
  // click steps back, and the pointer's bucket is read out on each. A click on a bucket, or enter on
  // one chosen with the arrow keys, opens the runs it counts. Since a canvas is a picture a screen
  // reader cannot read, the numbers drawn are in a table under it, and each series in its legend by
  // name, so that no series is told by its colour alone.
  let {
    title,
    since,
    width,
    series,
    stacked = false,
    limit,
    format,
    onzoom,
    onpick,
    onback,
    height = 220,
  }: {
    title: string;
    since: string[];
    width: number;
    series: Series[];
    stacked?: boolean;
    limit?: { label: string; value: number };
    format: (v: number) => string;
    onzoom?: (from: Date, to: Date) => void;
    onpick?: (bucket: number) => void;
    onback?: () => void;
    height?: number;
  } = $props();

  // The longest series' name in characters, which sets how narrow a column of the legend may be, so
  // that a name such as "refused for max_runs_per_hour" is never broken where its column ends.
  const longest = $derived(series.reduce((n, s) => Math.max(n, s.label.length), 0));

  let holder: HTMLDivElement | undefined = $state();
  let chosen = $state<number | null>(null);
  // pointed is the bucket under the pointer, which the legend reads out, or the one the arrow keys
  // chose where the pointer is over none.
  let pointed = $state<number | null>(null);
  const at = $derived(pointed ?? chosen);
  const valueAt = (s: Series, i: number) => (s.values[i] === null || s.values[i] === undefined ? "none" : format(s.values[i]!));

  const tones: Record<Tone, [string, number]> = {
    succeeded: ["--succeeded", 1],
    failed: ["--failed", 1],
    running: ["--running", 1],
    waiting: ["--waiting", 1],
    quiet: ["--faint", 1],
    accent: ["--accent", 1],
    "accent-2": ["--accent", 0.6],
    "accent-3": ["--accent", 0.32],
  };

  // colour reads a token of the ground in use, as the chart draws, since a canvas takes a colour and
  // not a custom property.
  function colour(name: string, alpha = 1): string {
    const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    if (alpha === 1 || !/^#[0-9a-f]{6}$/i.test(value)) return value;
    const n = parseInt(value.slice(1), 16);
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
  }

  function x(): number[] {
    return since.map((s) => Date.parse(s) / 1000 + width / 2);
  }

  // What each series draws: its values, or for a stack, the sum of it and every series under it. Only
  // columns stack: a line, the span before dashed or another count beside the columns, is drawn over
  // the stack and not on it.
  function drawn(): (number | null)[][] {
    if (!stacked) return series.map((s) => s.values);
    const sums: (number | null)[][] = [];
    let under: number[] = since.map(() => 0);
    for (const s of series) {
      if (s.dashed || s.kind !== "bars") {
        sums.push(s.values);
        continue;
      }
      under = under.map((u, i) => u + (s.values[i] ?? 0));
      sums.push([...under]);
    }
    return sums;
  }

  // ticks writes the time axis in UTC, as the buckets fall: the hour, and the day under the first
  // tick of each day; the day alone where the ticks are a day or more apart.
  function ticks(splits: number[]): string[] {
    const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
    const daily = splits.length > 1 && splits[1]! - splits[0]! >= 86_400;
    let day = -1;
    return splits.map((v) => {
      const d = new Date(v * 1000);
      const date = `${d.getUTCDate()} ${months[d.getUTCMonth()]}`;
      if (daily) return date;
      const time = `${String(d.getUTCHours()).padStart(2, "0")}:${String(d.getUTCMinutes()).padStart(2, "0")}`;
      if (d.getUTCDate() !== day) {
        day = d.getUTCDate();
        return `${time}\n${date}`;
      }
      return time;
    });
  }

  function bounds(i: number): string {
    const from = new Date(Date.parse(since[i] ?? "")).toISOString().slice(0, 16).replace("T", " ");
    const to = new Date(Date.parse(since[i] ?? "") + width * 1000).toISOString().slice(11, 16);
    return `${from} to ${to} UTC`;
  }

  // A stack is drawn from its top down, so that each series covers the part of the one above it
  // that is not its own.
  const ordered = () => (stacked ? [...series.keys()].reverse() : [...series.keys()]);

  // dataOf is what uPlot draws: the buckets' middles, then each series in the order drawn.
  function dataOf(): uPlot.AlignedData {
    const ys = drawn();
    return [x(), ...ordered().map((i) => ys[i]!)];
  }

  // shapeOf is what a chart is built for, beside its numbers: its series and how each is drawn. New
  // numbers in the same shape are drawn in place, as a live chart's are each time it is read; a new
  // shape builds the chart again.
  const shapeOf = () => `${stacked} ${height} ${series.map((s) => `${s.label}/${s.kind}/${s.tone}/${s.dashed ?? false}`).join(" ")}`;

  // gutter is the width of the values' axis: the widest label of this chart, measured in the axis'
  // font, with uPlot's 5px gap and a pixel, or the widest any chart drawn with it needs, and nothing
  // more, so that the labels start on the pane's edge as the text above them does.
  const me = Symbol("chart");
  function gutter(u: uPlot, values: string[] | null): number {
    if (values && values.length > 0) {
      const need = Math.ceil(widest(u, values)) + 6;
      if (needs.get(me) !== need) needs.set(me, need);
    }
    return needs.size > 0 ? Math.max(...needs.values()) : 0;
  }

  // widest is the widest of labels in the axes' font, in CSS pixels, a label of two lines by its
  // longer.
  function widest(u: uPlot, labels: string[]): number {
    u.ctx.font = `${12 * devicePixelRatio}px Archivo, sans-serif`;
    return Math.max(0, ...labels.flatMap((l) => l.split("\n")).map((l) => u.ctx.measureText(l).width)) / devicePixelRatio;
  }

  // fitted leaves out a time whose label, centred on its tick, would run past the chart's left edge,
  // which the values' axis, as wide as its own labels, may leave less than half a time from the plot.
  function fitted(u: uPlot, splits: number[], labels: string[]): string[] {
    const left = (u.bbox?.left ?? 0) / devicePixelRatio;
    return labels.map((label, i) => (left + u.valToPos(splits[i]!, "x") < widest(u, [label]) / 2 ? "" : label));
  }

  // Laid out again when another chart's labels widen or narrow the axis they share.
  $effect(() => {
    void [...needs.values()];
    untrack(() => {
      if (plot && holder) plot.setSize({ width: holder.clientWidth, height });
    });
  });

  function build(el: HTMLDivElement): uPlot {
    const bars = uPlot.paths.bars!({ size: [0.7, 60] });
    const stepped = uPlot.paths.stepped!({ align: 1 });
    const order = ordered();
    const opts: uPlot.Options = {
      width: el.clientWidth || 600,
      height,
      padding: [12, 8, 0, 0],
      cursor: {
        sync: { key: "statistics" },
        drag: { x: true, y: false, setScale: false },
        points: { size: 6 },
      },
      select: { show: true, left: 0, top: 0, width: 0, height: 0 },
      // The legend is the chart's own, below it, at a size that never changes: uPlot's grows and
      // shrinks with the values it reads out, which moves the page under the pointer.
      legend: { show: false },
      // Above the highest value, a twelfth more; and where a limit is drawn, room for its label over
      // its line as well, about 24px of the plot whatever its height, rather than a share of the
      // limit, which left the label against the top and its line through the letters.
      scales: { x: { time: true }, y: { range: (_u, min, max) => [Math.min(0, min), Math.max(max, limit?.value ?? 0) * (limit ? 1 + 24 / Math.max(48, height - 60) : 1.08) || 1] } },
      axes: [
        { stroke: colour("--muted"), grid: { stroke: colour("--line"), width: 1 }, ticks: { stroke: colour("--line") }, values: (u, splits) => fitted(u, splits, ticks(splits)), font: "12px Archivo, sans-serif" },
        { stroke: colour("--muted"), grid: { stroke: colour("--line"), width: 1 }, ticks: { show: false }, size: (u, values) => gutter(u, values), values: (_u, vals) => vals.map((v) => format(v)), font: "12px Archivo, sans-serif" },
      ],
      series: [
        { label: "bucket", value: (_u, _v, _si, i) => (i === null || i === undefined ? "" : bounds(i)) },
        ...order.map((i) => {
          const s = series[i]!;
          const [name, alpha] = tones[s.tone];
          const stroke = colour(name, alpha);
          return {
            label: s.label,
            stroke,
            width: s.kind === "bars" ? 0 : 1.5,
            dash: s.dashed ? [5, 4] : undefined,
            fill: s.kind === "bars" || s.kind === "area" ? colour(name, s.kind === "area" ? 0.14 : alpha) : undefined,
            paths: s.kind === "bars" ? bars : s.kind === "step" ? stepped : undefined,
            points: { show: false },
            value: (_u: uPlot, _v: number | null, _si: number, idx: number | null) => {
              if (idx === null) return "";
              const raw = s.values[idx];
              return raw === null || raw === undefined ? "none" : format(raw);
            },
          } satisfies uPlot.Series;
        }),
      ],
      hooks: {
        setCursor: [(u) => (pointed = u.cursor.idx ?? null)],
        setSelect: [
          (u) => {
            if (u.select.width > 4 && onzoom) {
              const from = u.posToVal(u.select.left, "x") - width / 2;
              const to = u.posToVal(u.select.left + u.select.width, "x") + width / 2;
              onzoom(new Date(from * 1000), new Date(to * 1000));
            }
            u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
          },
        ],
        draw: [
          (u) => {
            if (!limit) return;
            const y = u.valToPos(limit.value, "y", true);
            const ctx = u.ctx;
            ctx.save();
            ctx.strokeStyle = colour("--failed");
            ctx.setLineDash([6, 4]);
            ctx.lineWidth = 1.2 * devicePixelRatio;
            ctx.beginPath();
            ctx.moveTo(u.bbox.left, y);
            ctx.lineTo(u.bbox.left + u.bbox.width, y);
            ctx.stroke();
            ctx.setLineDash([]);
            ctx.fillStyle = colour("--failed");
            ctx.font = `${12 * devicePixelRatio}px Archivo, sans-serif`;
            ctx.textAlign = "right";
            ctx.textBaseline = "bottom";
            ctx.fillText(`${limit.label} ${format(limit.value)}`, u.bbox.left + u.bbox.width - 6 * devicePixelRatio, y - 4 * devicePixelRatio);
            ctx.restore();
          },
        ],
      },
    };
    return new uPlot(opts, dataOf(), el);
  }

  let plot: uPlot | undefined;
  let built = "";
  let rebuild = () => {};

  // New numbers, read again on a live chart, are drawn into the chart there is.
  $effect(() => {
    void [series, since, width, limit];
    untrack(() => {
      if (!plot) return;
      if (shapeOf() !== built) rebuild();
      else plot.setData(dataOf());
    });
  });

  onMount(() => {
    if (!holder) return;
    const el = holder;
    plot = build(el);
    built = shapeOf();
    let down = 0;

    // A click with no drag opens the runs of the bucket under the pointer.
    const pressed = (e: MouseEvent) => (down = e.clientX);
    const released = (e: MouseEvent) => {
      if (Math.abs(e.clientX - down) < 4 && plot?.cursor.idx !== null && plot?.cursor.idx !== undefined) {
        onpick?.(plot.cursor.idx);
      }
    };
    const twice = () => onback?.();
    const listen = () => {
      plot?.over.addEventListener("mousedown", pressed);
      plot?.over.addEventListener("mouseup", released);
      plot?.over.addEventListener("dblclick", twice);
    };
    listen();

    // Drawn again at the new size when the page is, and in the new ground's colours when it changes.
    const resized = new ResizeObserver(() => plot?.setSize({ width: el.clientWidth, height }));
    resized.observe(el);
    const redraw = () => {
      plot?.destroy();
      plot = build(el);
      built = shapeOf();
      listen();
    };
    rebuild = redraw;
    const system = matchMedia("(prefers-color-scheme: dark)");
    system.addEventListener("change", redraw);
    const ground = new MutationObserver(redraw);
    ground.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

    return () => {
      resized.disconnect();
      system.removeEventListener("change", redraw);
      ground.disconnect();
      plot?.destroy();
      plot = undefined;
      needs.delete(me);
    };
  });

  // The arrow keys move the readout a bucket at a time, and enter opens the runs of that bucket.
  function key(e: KeyboardEvent) {
    const last = since.length - 1;
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      const from = chosen ?? (e.key === "ArrowRight" ? -1 : last + 1);
      chosen = Math.max(0, Math.min(last, from + (e.key === "ArrowRight" ? 1 : -1)));
    } else if (e.key === "Enter" && chosen !== null) {
      onpick?.(chosen);
    }
  }
</script>

<figure class="chart">
  <figcaption class="unseen">{title}</figcaption>
  <!-- A slider over the buckets, as the arrow keys move it; what it draws is in the table below. -->
  <div
    class="plot"
    bind:this={holder}
    tabindex="0"
    role="slider"
    aria-label="{title}: {series.map((s) => s.label).join(', ')}. The table below holds its numbers."
    aria-valuemin={0}
    aria-valuemax={Math.max(0, since.length - 1)}
    aria-valuenow={chosen ?? 0}
    aria-valuetext={chosen === null ? "no bucket chosen" : bounds(chosen)}
    onkeydown={key}
  ></div>
  <!-- The series in columns of one width, each value read out at its column's end, and the bucket
       read out under them, its line kept while nothing is pointed at so that nothing moves. -->
  <div class="legend" aria-live="polite" style:--longest="{longest}ch">
    <ul>
      {#each series as s (s.label)}
        <li><span class="swatch {s.tone}" class:line={s.kind === "line" || s.kind === "step"} class:dashed={s.dashed}></span><span class="name">{s.label}</span><span class="value term">{at !== null && since[at] ? valueAt(s, at) : ""}</span></li>
      {/each}
    </ul>
    <p class="bucket term">{at !== null && since[at] ? bounds(at) : "\u00a0"}</p>
  </div>
  <details>
    <summary>Numbers</summary>
    <table>
      <thead>
        <tr><th>Bucket</th>{#each series as s (s.label)}<th class="number">{s.label}</th>{/each}</tr>
      </thead>
      <tbody>
        {#each since as at, i (at)}
          <tr>
            <td class="term">{bounds(i)}</td>
            {#each series as s (s.label)}<td class="number term">{s.values[i] === null || s.values[i] === undefined ? "" : format(s.values[i]!)}</td>{/each}
          </tr>
        {/each}
      </tbody>
    </table>
  </details>
</figure>

<style>
  .chart {
    margin: 0;
  }

  .plot {
    border-radius: var(--radius-control);
  }

  /* uPlot sets its own leading of 1.5, a fraction at 13.5px: the console's, in whole units, puts its
     axes on pixels. */
  .plot :global(.uplot) {
    line-height: round(calc(var(--leading) * 1em), var(--unit));
  }

  .plot :global(.u-select) {
    background: var(--accentDim);
  }

  .plot :global(.u-cursor-x),
  .plot :global(.u-cursor-y) {
    border-color: var(--lineStrong);
  }

  .legend {
    margin-top: calc(var(--unit) * 3);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .bucket {
    margin: calc(var(--unit) * 2) 0 0;
    color: var(--text);
  }

  /* Columns of one width, as many as the pane holds, so that the second column of every row starts
     where the first row's does, each wide enough for the longest name beside its swatch and its value:
     at 140px a column broke max_runs_per_hour in two. */
  .legend ul {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, max(140px, calc(var(--longest, 0ch) + 10px + var(--unit) * 4 + 7ch))), 1fr));
    gap: calc(var(--unit) * 2) calc(var(--unit) * 7);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .legend li {
    display: flex;
    align-items: center;
    min-width: 0;
  }

  .name {
    min-width: 0;
    overflow-wrap: break-word;
  }

  /* A value has its room whether it is read out or not, so that nothing moves as the pointer does,
     at the end of its column, where the values of a column line up. */
  .value {
    flex: none;
    min-width: 6ch;
    margin-left: auto;
    padding-left: calc(var(--unit) * 2);
    color: var(--text);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  /* A swatch has the series' shape as well as its colour: a square for columns and areas, a stroke
     for a line, dashed for the span before, so that two series of one hue, the runs running and the
     tasks in flight say, are told apart. */
  .swatch {
    flex: none;
    width: 10px;
    height: 10px;
    margin-right: calc(var(--unit) * 2);
    border-radius: 2px;
  }

  .swatch.line,
  .swatch.dashed {
    height: 0;
    border-top: 2px solid;
    border-radius: 0;
    background: none !important;
  }

  .swatch.dashed {
    border-top-style: dashed;
  }

  .swatch.succeeded { background: var(--succeeded); border-color: var(--succeeded); }
  .swatch.failed { background: var(--failed); border-color: var(--failed); }
  .swatch.running { background: var(--running); border-color: var(--running); }
  .swatch.accent { background: var(--accent); border-color: var(--accent); }
  .swatch.waiting { background: var(--waiting); border-color: var(--waiting); }
  .swatch.quiet { background: var(--faint); border-color: var(--faint); }
  .swatch.accent-2 { background: var(--accentLine); border-color: var(--accentLine); }
  .swatch.accent-3 { background: var(--accentDim); border-color: var(--accentLine); }

  details {
    margin-top: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  summary {
    color: var(--muted);
    cursor: pointer;
  }

  table {
    width: 100%;
    margin-top: calc(var(--unit) * 3);
    border-collapse: collapse;
  }

  th,
  td {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    text-align: left;
  }

  th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .number {
    text-align: right;
  }
</style>
