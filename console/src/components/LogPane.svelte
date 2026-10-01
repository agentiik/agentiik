<script lang="ts">
  import Problem from "./Problem.svelte";
  import type { API } from "../api/client";
  import { LogTail } from "../lib/logs.svelte";

  // The log of the task chosen, as the step's stream gives it: every dispatch of that task in turn,
  // each line with its number, history then live until the step's log is over. The lines are the
  // runner's, secrets masked before anything was written, so they are shown as they are.
  let { api, run, step, task }: { api: API; run: string; step: string; task: string } = $props();

  // How many of a dispatch's last lines are drawn at once, and how many more each ask draws: a log is
  // up to log_max_lines, and every line drawn is a node of the page.
  const batch = 500;

  const tail = $derived(new LogTail(api, run, step));
  let drawn = $state(batch);

  $effect(() => {
    const t = tail;
    t.open(new URL(`api/v1/runs/${encodeURIComponent(run)}/steps/${encodeURIComponent(step)}/logs`, document.baseURI).href);
    drawn = batch;
    return () => t.close();
  });

  const dispatches = $derived(tail.of(task));
  const live = $derived(tail.verdict === null && tail.refused === null);
</script>

<section class="log" aria-label="Log of the task chosen">
  <p class="state muted">
    {#if tail.refused}
      <Problem explained={tail.refused} />
    {:else if tail.reconnecting}
      Reconnecting
    {:else if live}
      <span class="dot" aria-hidden="true"></span> Live
    {:else}
      Finished: {tail.verdict}
    {/if}
  </p>

  {#each dispatches as d (d.id)}
    {@const first = Math.max(0, d.lines.length - drawn)}
    {#if dispatches.length > 1}
      <h4 class="term">dispatch {d.requeue + 1} of attempt {d.attempt}{#if d.requeue > 0}, handed out again{/if}</h4>
    {/if}
    {#if first > 0}
      <button class="control" onclick={() => (drawn += batch)}>Show earlier lines</button>
    {/if}
    {#if d.lines.length === 0 && d.gaps.length === 0}
      <p class="muted">{d.over ? "This dispatch wrote nothing to its log." : "Nothing written yet."}</p>
    {:else}
      <pre class="lines"><code>{#each d.lines.slice(first) as l (l.line)}<span class="line"><span class="n" aria-hidden="true">{l.line}</span>{l.text}
</span>{/each}</code></pre>
    {/if}
    {#each d.gaps as g (g.first)}
      <p class="gap">Lines {g.first} to {g.first + g.lines - 1} unavailable: {g.reason}</p>
    {/each}
    {#if d.over?.truncated}
      <p class="muted">Log truncated</p>
    {/if}
    {#if d.over && !d.over.final}
      <p class="muted">Log incomplete</p>
    {/if}
  {:else}
    {#if !live && !tail.refused}
      <p class="muted">No log</p>
    {:else if !tail.refused}
      <p class="muted">Not started</p>
    {/if}
  {/each}
</section>

<style>
  .log {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 4);
  }

  .state {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    margin: 0;
  }

  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--running);
  }


  h4 {
    margin: calc(var(--unit) * 2) 0 0;
    font-size: var(--type-identifier-size-max);
    font-weight: 600;
  }

  .lines {
    max-height: 420px;
    margin: 0;
    padding: calc(var(--unit) * 4) calc(var(--unit) * 5);
    overflow: auto;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-family: var(--type-identifier-font);
    font-size: 12px;
    --leading: 1.55;
    white-space: pre-wrap;
    word-break: break-all;
  }

  .line {
    display: block;
  }

  .n {
    display: inline-block;
    min-width: 5ch;
    margin-right: 2ch;
    color: var(--faint);
    text-align: right;
    user-select: none;
  }

  .gap {
    margin: 0;
    color: var(--waiting);
  }

  p {
    margin: 0;
  }
</style>
