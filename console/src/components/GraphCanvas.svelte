<script lang="ts">
  import { took } from "../lib/format";
  import { chip, node, scheduling, what, type Graph, type Layout } from "../lib/graph";
  import { lastAttempt, type RunDetail } from "../lib/run.svelte";
  import Icon from "./Icon.svelte";
  import StatePill from "./StatePill.svelte";

  // A workflow's graph, drawn top to bottom from its layout: each step a box naming what it runs and
  // what decides when it runs, its ports as dots named beside them, each edge from the port it leaves
  // to the port it arrives on, and the workflow's inputs and outputs as chips at the boundary. Where
  // a run is shown, each step carries its state as a dot and a word, a fan-out its shards as cells
  // and a progress bar, and each edge the items its port published. A box is a button: choosing one
  // selects its step, which the file beside it highlights.
  let {
    graph,
    laid,
    run,
    selected,
    onselect,
  }: { graph: Graph; laid: Layout; run: RunDetail | null; selected: string | undefined; onselect: (step: string) => void } = $props();

  let zoom = $state(1);
  let holder: HTMLDivElement | undefined = $state();

  const margin = 32;
  const states = ["succeeded", "running", "pending", "failed", "skipped"];

  function fit() {
    if (!holder) return;
    const w = holder.clientWidth - margin * 2;
    zoom = Math.max(0.3, Math.min(1.5, Math.round((w / laid.width) * 20) / 20));
  }

  const stepOf = (name: string) => run?.steps.find((s) => s.step === name);
  const items = (step: string, port: string) => stepOf(step)?.ports?.[port]?.items;

  function lasted(name: string): string {
    const s = stepOf(name);
    if (!s?.started_at) return "";
    const end = s.finished_at ? Date.parse(s.finished_at) : Date.now();
    return took(Math.max(0, end - Date.parse(s.started_at)));
  }

  // The shards of a fan-out as the run's last attempt of each left them, and how many have ended.
  function shards(name: string) {
    if (!run) return undefined;
    const tasks = lastAttempt(run, name).filter((t) => t.shard);
    if (tasks.length === 0) return undefined;
    const of = tasks[0]!.shard!.of;
    const ended = tasks.filter((t) => ["succeeded", "failed", "timed_out", "cancelled", "lost"].includes(t.state)).length;
    return { tasks, of, ended };
  }

  const called = (name: string) => run?.tasks.find((t) => t.step === name && t.called)?.called;
</script>

<div class="canvas" bind:this={holder}>
  <div class="scroll">
    <div class="plane" style:width="{(laid.width + margin * 2) * zoom}px" style:height="{(laid.height + margin * 2) * zoom}px">
      <div class="scaled" style:transform="scale({zoom})" style:width="{laid.width + margin * 2}px" style:height="{laid.height + margin * 2}px">
        <svg class="edges" width={laid.width + margin * 2} height={laid.height + margin * 2} aria-hidden="true">
          <g transform="translate({margin} {margin})">
            {#each laid.inputs as c (c.name)}
              {#if c.path}<path class="edge data" d={c.path} />{/if}
            {/each}
            {#each laid.edges as e (`${e.from.step}.${e.from.port}>${e.to.step}.${e.to.port}`)}
              <path class="edge {e.kind}" d={e.path} />
            {/each}
            {#each laid.outputs as c (c.name)}
              {#if c.path}<path class="edge {c.from?.port === 'error' ? 'error' : c.from?.port === 'rejected' ? 'rejected' : 'data'}" d={c.path} />{/if}
            {/each}
          </g>
        </svg>
        <div class="layer" style:left="{margin}px" style:top="{margin}px">
          {#each laid.inputs as c (c.name)}
            <span class="chip term" style:left="{c.x}px" style:top="{c.y}px" style:width="{c.width}px" style:height="{chip.height}px" title="workflow input {c.name}{graph.inputs?.[c.name]?.required ? ', required' : ''}">{c.name}</span>
          {/each}
          {#each laid.outputs as c (c.name)}
            {@const o = graph.outputs?.[c.name]}
            <span
              class="chip term"
              style:left="{c.x}px"
              style:top="{c.y}px"
              style:width="{c.width}px"
              style:height="{chip.height}px"
              title="workflow output {c.name}: {o?.from.step}.{o?.from.port}{o?.retain?.for ? `, kept ${o.retain.for}` : ''}{o?.retain?.fetches ? `, ${o.retain.fetches} fetches` : ''}">{c.name}</span>
            {#if o}<span class="from faint term" style:left="{c.x}px" style:top="{c.y + chip.height + 4}px" style:width="{c.width}px">{o.from.step}.{o.from.port}{o.retain?.for ? ` · ${o.retain.for}` : ""}</span>{/if}
          {/each}
          {#each laid.edges as e (`${e.from.step}.${e.from.port}>${e.to.step}.${e.to.port}`)}
            {@const n = items(e.from.step, e.from.port)}
            {#if n !== undefined}<span class="count term {e.kind}" style:left="{e.mid.x + 6}px" style:top="{e.mid.y - 8}px">{n} items</span>{/if}
          {/each}
          {#each laid.nodes as p (p.step)}
            {@const s = graph.steps[p.step]!}
            {@const st = stepOf(p.step)}
            {@const sh = shards(p.step)}
            {@const marks = scheduling(s)}
            {@const child = called(p.step)}
            <button
              class="node {st ? st.verdict : ''} {s.kind}"
              class:chosen={selected === p.step}
              style:left="{p.x}px"
              style:top="{p.y}px"
              style:width="{node.width}px"
              style:height="{node.height}px"
              aria-pressed={selected === p.step}
              aria-label="Step {p.step}, {s.kind}{st ? `, ${st.verdict}` : ''}"
              onclick={() => onselect(p.step)}
            >
              <span class="head">
                <span class="term name">{p.step}</span>
                {#if st}<span class="took term">{lasted(p.step)}</span>{/if}
              </span>
              <span class="line">
                {#if st}<StatePill state={st.verdict} />{/if}
                <span class="term muted what" title={s.image ?? (s.workflow ? `${s.workflow.workflow}@${s.workflow.ref ?? "default branch"}` : "")}>{what(s)}</span>
              </span>
              {#if sh}
                <span class="shards" aria-label="{sh.ended} of {sh.of} shards ended">
                  {#each sh.tasks as t (t.task)}<span class="cell {t.state}" title="shard {t.shard?.index}: {t.state}"></span>{/each}
                  <span class="bar"><span class="fill" style:width="{(sh.ended / sh.of) * 100}%"></span></span>
                  <span class="term muted">{sh.ended} of {sh.of}</span>
                </span>
              {:else if child}
                <span class="line term muted">child run {child}</span>
              {:else if marks.length}
                <span class="line term faint marks" title={marks.join(" · ")}>{marks.join(" · ")}</span>
              {/if}
            </button>
            {#each p.inputs as port (port.name)}
              <span class="port" style:left="{port.x - 4}px" style:top="{port.y - 4}px" aria-hidden="true"></span>
              <span class="portname term" style:left="{port.x + 7}px" style:top="{port.y - 19}px">{port.name}</span>
            {/each}
            {#each p.outputs as port (port.name)}
              <span class="port {port.name === 'error' ? 'error' : port.name === 'rejected' ? 'rejected' : ''}" style:left="{port.x - 4}px" style:top="{port.y - 4}px" aria-hidden="true"></span>
              <span class="portname term" style:left="{port.x + 7}px" style:top="{port.y + 3}px">{port.name}</span>
            {/each}
          {/each}
        </div>
      </div>
    </div>
  </div>

  <div class="bar-bottom">
    <button class="control" aria-label="Zoom out" onclick={() => (zoom = Math.max(0.3, Math.round((zoom - 0.1) * 10) / 10))}><Icon name="control-zoom_out" size={14} /></button>
    <span class="term zoom">{Math.round(zoom * 100)}%</span>
    <button class="control" aria-label="Zoom in" onclick={() => (zoom = Math.min(2, Math.round((zoom + 0.1) * 10) / 10))}><Icon name="control-zoom_in" size={14} /></button>
    <button class="control" onclick={fit}><Icon name="control-fit" size={14} />Fit</button>
    <span class="legend" aria-label="Legend">
      {#each states as s (s)}<StatePill state={s} live={false} />{/each}
    </span>
  </div>
</div>

<style>
  .canvas {
    display: flex;
    flex-direction: column;
    min-height: 0;
  }

  .scroll {
    overflow: auto;
    min-height: 360px;
    max-height: 72vh;
    border-radius: var(--radius-control);
    background-color: var(--sunken);
    background-image: radial-gradient(var(--line) 1px, transparent 1px);
    background-size: 16px 16px;
  }

  .plane {
    position: relative;
    margin: 0 auto;
  }

  .scaled {
    position: absolute;
    top: 0;
    left: 0;
    transform-origin: 0 0;
  }

  .edges {
    position: absolute;
    top: 0;
    left: 0;
    overflow: visible;
  }

  .edge {
    fill: none;
    stroke: var(--lineStrong);
    stroke-width: 1.5;
  }

  .edge.rejected {
    stroke: var(--waiting);
    stroke-dasharray: 5 4;
  }

  .edge.error {
    stroke: var(--failed);
  }

  .layer {
    position: absolute;
  }

  .layer > * {
    position: absolute;
  }

  .chip {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-round);
    background: var(--raised);
    font-size: 12px;
  }

  .from {
    font-size: 11px;
    text-align: center;
  }

  .count {
    color: var(--muted);
    font-size: 11px;
    white-space: nowrap;
  }

  .count.rejected {
    color: var(--waiting);
  }

  .count.error {
    color: var(--failed);
  }

  .node {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: calc(var(--unit) * 3);
    padding: calc(var(--unit) * 5) calc(var(--unit) * 6);
    overflow: hidden;
    border: var(--border-node) solid var(--lineStrong);
    border-radius: var(--radius-innerPanel);
    background: var(--surface);
    color: var(--text);
    text-align: left;
    cursor: pointer;
  }

  .node.succeeded {
    border-color: color-mix(in srgb, var(--succeeded) 60%, var(--lineStrong));
  }

  .node.running {
    border-color: var(--accentLine);
  }

  .node.failed {
    border-color: var(--failed);
  }

  .node.chosen {
    border: var(--border-focus) solid var(--accent);
    padding: calc(var(--unit) * 5 - 1px) calc(var(--unit) * 6 - 1px);
  }

  .node:focus-visible {
    outline: var(--border-focus) solid var(--accent);
    outline-offset: 2px;
  }

  .head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: calc(var(--unit) * 4);
  }

  .name {
    overflow: hidden;
    font-size: var(--type-identifier-size-max);
    font-weight: 600;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .took {
    color: var(--muted);
    font-size: 12px;
  }

  .line {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    min-width: 0;
    font-size: 12px;
  }

  .what,
  .marks {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .shards {
    display: flex;
    align-items: center;
    gap: 3px;
    font-size: 12px;
  }

  .cell {
    width: 9px;
    height: 12px;
    border-radius: 2px;
    background: var(--faint);
  }

  .cell.succeeded {
    background: var(--succeeded);
  }

  .cell.running,
  .cell.dispatched,
  .cell.publishing {
    background: var(--accent);
  }

  .cell.failed,
  .cell.timed_out,
  .cell.lost {
    background: var(--failed);
  }

  .bar {
    flex: 1;
    height: 6px;
    margin: 0 calc(var(--unit) * 3);
    overflow: hidden;
    border-radius: var(--radius-round);
    background: var(--line);
  }

  .fill {
    display: block;
    height: 100%;
    background: var(--succeeded);
  }

  .port {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent);
  }

  .port.rejected {
    background: var(--waiting);
  }

  .port.error {
    background: var(--failed);
  }

  .portname {
    color: var(--muted);
    font-size: 11px;
    white-space: nowrap;
  }

  .bar-bottom {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    padding-top: calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  .zoom {
    min-width: 40px;
    text-align: center;
  }

  .legend {
    display: inline-flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3);
    margin-left: calc(var(--unit) * 6);
  }
</style>
