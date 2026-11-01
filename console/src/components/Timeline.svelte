<script lang="ts">
  import { clock, took } from "../lib/format";
  import type { RunDetail } from "../lib/run.svelte";
  import { ticks, timeline } from "../lib/timeline";

  // The run's timeline, under its graph: a lane for each step, each attempt a span on it from its
  // first shard's start to its last shard's end, against the run's own span, with round marks from
  // its start under the lanes. A step's name chooses it, as its box on the graph does.
  let { run, now, chosen, onselect }: { run: RunDetail; now: number; chosen: string | undefined; onselect: (step: string) => void } = $props();

  const drawn = $derived(timeline(run, now));
  const marks = $derived(ticks(drawn.length));
  const at = (ms: number) => `${(ms / drawn.length) * 100}%`;
  const end = $derived(run.finished_at ?? new Date(now).toISOString());
</script>

<section class="timeline" aria-label="Timeline">
  <h3>
    Timeline
    {#if run.started_at}<span class="term faint"><time datetime={run.started_at}>{clock(run.started_at, now)}</time> to <time datetime={end}>{clock(end, now)}</time></span>{/if}
  </h3>
  <div class="lanes">
    {#each drawn.lanes as lane (lane.step)}
      <button class="name term" class:chosen={lane.step === chosen} class:unreached={lane.spans.length === 0} aria-pressed={lane.step === chosen} onclick={() => onselect(lane.step)}>{lane.step}</button>
      <div class="lane">
        {#each marks as m (m.at)}<span class="rule" style:left={at(m.at)} aria-hidden="true"></span>{/each}
        {#each lane.spans as s (s.attempt)}
          <span class="span {s.tone}" class:retried={s.retried} style:left={at(s.from)} style:width={at(s.to - s.from)} title="{lane.step}, attempt {s.attempt}: {s.tone}, {took(s.to - s.from)}"></span>
        {/each}
      </div>
    {/each}
    <span></span>
    <div class="marks" aria-hidden="true">
      {#each marks as m (m.at)}<span class="term" style:left={at(m.at)}>{m.label}</span>{/each}
    </div>
  </div>
</section>

<style>
  .timeline {
    padding-top: calc(var(--unit) * 6);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
  }

  h3 {
    display: flex;
    gap: calc(var(--unit) * 5);
    margin: 0 0 calc(var(--unit) * 5);
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 600;
  }

  h3 span {
    font-weight: 400;
  }

  .lanes {
    display: grid;
    grid-template-columns: minmax(0, max-content) minmax(0, 1fr);
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    align-items: center;
  }

  .name {
    max-width: 160px;
    overflow: hidden;
    padding: 0;
    border: none;
    background: none;
    color: var(--text);
    font-size: var(--type-control-size);
    text-align: left;
    text-overflow: ellipsis;
    white-space: nowrap;
    cursor: pointer;
  }

  .name.unreached {
    color: var(--faint);
  }

  .name.chosen {
    color: var(--accent);
  }

  /* A lane is as tall as a line of its names, so that a name and its spans share a middle. */
  .lane {
    position: relative;
    height: 20px;
  }

  .rule {
    position: absolute;
    top: 0;
    bottom: 0;
    width: var(--border-hairline);
    background: var(--line);
  }

  .span {
    position: absolute;
    top: 5px;
    min-width: 3px;
    height: 10px;
    border-radius: var(--radius-round);
  }

  .span.succeeded {
    background: var(--succeeded);
  }

  .span.failed {
    background: var(--failed);
  }

  .span.running {
    background: var(--running);
  }

  .span.cancelled {
    background: var(--faint);
  }

  /* An attempt another came after is drawn paler than the one the step ended on, which alone carries
     its verdict. */
  .span.retried {
    opacity: 0.4;
  }

  .marks {
    position: relative;
    height: 16px;
  }

  .marks span {
    position: absolute;
    color: var(--faint);
    font-size: 11px;
    transform: translateX(-50%);
    white-space: nowrap;
  }

  .marks span:first-child {
    transform: none;
  }

  .marks span:last-child:not(:first-child) {
    transform: translateX(-100%);
  }
</style>
