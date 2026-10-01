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
</script>

<script lang="ts">
  import { onMount } from "svelte";
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

  let holder: HTMLDivElement | undefined = $state();
  let chosen = $state<number | null>(null);

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

  // What each series draws: its values, or for a stack, the sum of it and every series under it. A
  // dashed series, the span before, is drawn behind the stack and not on it.
  function drawn(): (number | null)[][] {
    if (!stacked) return series.map((s) => s.values);
    const sums: (number | null)[][] = [];
    let under: number[] = since.map(() => 0);
    for (const s of series) {
      if (s.dashed) {
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

  function build(el: HTMLDivElement): uPlot {
    const ys = drawn();
    const bars = uPlot.paths.bars!({ size: [0.7, 60] });
    const stepped = uPlot.paths.stepped!({ align: 1 });
    // A stack is drawn from its top down, so that each series covers the part of the one above it
    // that is not its own.
    const order = stacked ? [...series.keys()].reverse() : [...series.keys()];
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
      legend: { live: true },
      scales: { x: { time: true }, y: { range: (_u, min, max) => [Math.min(0, min), Math.max(max, limit?.value ?? 0) * 1.08 || 1] } },
      axes: [
        { stroke: colour("--muted"), grid: { stroke: colour("--line"), width: 1 }, ticks: { stroke: colour("--line") }, values: (_u, splits) => ticks(splits), font: "12px Archivo, sans-serif" },
        { stroke: colour("--muted"), grid: { stroke: colour("--line"), width: 1 }, ticks: { show: false }, size: 64, values: (_u, vals) => vals.map((v) => format(v)), font: "12px Archivo, sans-serif" },
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
            ctx.fillText(`${limit.label} ${format(limit.value)}`, u.bbox.left + u.bbox.width - 6 * devicePixelRatio, y - 5 * devicePixelRatio);
            ctx.restore();
          },
        ],
      },
    };
    const data: uPlot.AlignedData = [x(), ...order.map((i) => ys[i]!)];
    return new uPlot(opts, data, el);
  }

  onMount(() => {
    if (!holder) return;
    const el = holder;
    let plot = build(el);
    let down = 0;

    // A click with no drag opens the runs of the bucket under the pointer.
    const pressed = (e: MouseEvent) => (down = e.clientX);
    const released = (e: MouseEvent) => {
      if (Math.abs(e.clientX - down) < 4 && plot.cursor.idx !== null && plot.cursor.idx !== undefined) {
        onpick?.(plot.cursor.idx);
      }
    };
    const twice = () => onback?.();
    const listen = () => {
      plot.over.addEventListener("mousedown", pressed);
      plot.over.addEventListener("mouseup", released);
      plot.over.addEventListener("dblclick", twice);
    };
    listen();

    // Drawn again at the new size when the page is, and in the new ground's colours when it changes.
    const resized = new ResizeObserver(() => plot.setSize({ width: el.clientWidth, height }));
    resized.observe(el);
    const redraw = () => {
      plot.destroy();
      plot = build(el);
      listen();
    };
    const system = matchMedia("(prefers-color-scheme: dark)");
    system.addEventListener("change", redraw);
    const ground = new MutationObserver(redraw);
    ground.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

    return () => {
      resized.disconnect();
      system.removeEventListener("change", redraw);
      ground.disconnect();
      plot.destroy();
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
  <figcaption>{title}</figcaption>
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
  {#if chosen !== null && since[chosen]}
    <p class="readout" aria-live="polite">
      <span class="term">{bounds(chosen)}</span>
      {#each series as s (s.label)}
        <span><span class="swatch {s.tone}" class:dashed={s.dashed}></span>{s.label} <span class="term">{s.values[chosen] === null ? "none" : format(s.values[chosen] ?? 0)}</span></span>
      {/each}
    </p>
  {/if}
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

  figcaption {
    margin-bottom: calc(var(--unit) * 3);
    font-family: var(--type-sectionTitle-font);
    font-size: var(--type-sectionTitle-size);
    font-weight: var(--type-sectionTitle-weight);
  }

  .plot {
    border-radius: var(--radius-control);
  }

  .plot :global(.u-legend) {
    margin-top: calc(var(--unit) * 3);
    color: var(--muted);
    font-family: var(--type-body-font);
    font-size: var(--type-control-size);
    text-align: left;
  }

  /* uPlot sets its own leading of 1.5 and a marker of 1em, both fractions at 13.5px: the console's
     leading in whole units, and a marker of 12px centred on a 20px line, put them on pixels. */
  .plot :global(.uplot) {
    line-height: round(calc(var(--leading) * 1em), var(--unit));
  }

  .plot :global(.u-legend th > *) {
    vertical-align: top;
  }

  .plot :global(.u-legend .u-marker) {
    width: 12px;
    height: 12px;
    margin-top: 4px;
  }

  .plot :global(.u-legend .u-value) {
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }

  /* A label with nothing beside it until the pointer is over the chart: no colon, and the bucket's own
     label only while it names one. */
  .plot :global(.u-legend th::after) {
    content: none;
  }

  .plot :global(.u-legend .u-value:not(:empty)) {
    padding-left: calc(var(--unit) * 2);
  }

  .plot :global(.u-legend .u-series:first-child:has(.u-value:empty)) {
    display: none;
  }

  .plot :global(.u-select) {
    background: var(--accentDim);
  }

  .plot :global(.u-cursor-x),
  .plot :global(.u-cursor-y) {
    border-color: var(--lineStrong);
  }

  .readout {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 7);
    margin: calc(var(--unit) * 3) 0 0;
    font-size: var(--type-control-size);
  }

  .swatch {
    display: inline-block;
    width: 10px;
    height: 10px;
    margin-right: calc(var(--unit) * 2);
    border-radius: 2px;
    vertical-align: -1px;
  }

  .swatch.dashed {
    height: 0;
    border-top: 2px dashed;
    background: none !important;
  }

  .swatch.succeeded { background: var(--succeeded); border-color: var(--succeeded); }
  .swatch.failed { background: var(--failed); border-color: var(--failed); }
  .swatch.running, .swatch.accent { background: var(--accent); border-color: var(--accent); }
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
