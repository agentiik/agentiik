<script lang="ts">
  import Problem from "./Problem.svelte";
  import type { API } from "../api/client";
  import Icon from "./Icon.svelte";
  import { asText, holding, LogTail } from "../lib/logs.svelte";

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

  // Find, over every line the stream delivered, each dispatch's kept apart; drawn 500 at a time from
  // the end as the whole log is.
  let finding = $state("");
  $effect(() => {
    void finding;
    drawn = batch;
  });
  const kept = $derived(dispatches.map((d) => holding(d.lines, finding)));
  const found = $derived(kept.reduce((n, k) => n + k.length, 0));
  const written = $derived(dispatches.some((d) => d.lines.length > 0));

  function download() {
    const link = document.createElement("a");
    link.href = URL.createObjectURL(new Blob([asText(dispatches)], { type: "text/plain" }));
    link.download = `${run}-${task.replaceAll("/", "-")}.log`;
    link.click();
    URL.revokeObjectURL(link.href);
  }
</script>

<section class="log" aria-label="Log of the task chosen">
  <div class="head">
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
    {#if written}
      <span class="tools">
        {#if finding}<span class="found muted term" role="status">{found === 1 ? "1 line" : `${found} lines`}</span>{/if}
        <label class="find">
          <Icon name="control-search" size={14} />
          <input type="search" bind:value={finding} placeholder="Find" aria-label="Find in the log" autocomplete="off" spellcheck="false" />
        </label>
        <button class="control" onclick={download}><Icon name="control-download" size={14} />Download the log</button>
      </span>
    {/if}
  </div>

  {#each dispatches as d, i (d.id)}
    {@const lines = kept[i] ?? []}
    {@const first = Math.max(0, lines.length - drawn)}
    {#if dispatches.length > 1}
      <h4 class="term">dispatch {d.requeue + 1} of attempt {d.attempt}{#if d.requeue > 0}, handed out again{/if}</h4>
    {/if}
    {#if first > 0}
      <button class="control" onclick={() => (drawn += batch)}>Show earlier lines</button>
    {/if}
    {#if d.lines.length === 0 && d.gaps.length === 0}
      <p class="muted">{d.over ? "This dispatch wrote nothing to its log." : "Nothing written yet."}</p>
    {:else if finding && lines.length === 0}
      <p class="muted">Nothing found</p>
    {:else}
      <pre class="lines"><code>{#each lines.slice(first) as { line: l, at } (l.line)}<span class="line"><span class="n" aria-hidden="true">{l.line}</span>{#if at >= 0}{l.text.slice(0, at)}<mark>{l.text.slice(at, at + finding.length)}</mark>{l.text.slice(at + finding.length)}{:else}{l.text}{/if}
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

  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
  }

  .state {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    margin: 0;
  }

  .found {
    white-space: nowrap;
  }

  /* Find gives up its width before anything else does, down to what holds a word. */
  .tools {
    display: flex;
    flex: 1 1 auto;
    align-items: center;
    justify-content: flex-end;
    gap: calc(var(--unit) * 4);
    min-width: 0;
  }

  .tools .control {
    flex: none;
  }

  .find {
    display: inline-flex;
    flex: 0 1 200px;
    align-items: center;
    gap: calc(var(--unit) * 3);
    min-width: 110px;
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--surface);
    color: var(--muted);
    cursor: text;
  }

  .find:focus-within {
    border-color: var(--accent);
  }

  .find input {
    flex: 1;
    min-width: 0;
    height: 100%;
    padding: 0;
    border: none;
    background: none;
    color: var(--text);
    font: inherit;
    font-size: var(--type-control-size);
    outline: none;
  }

  .find input::placeholder {
    color: var(--faint);
  }

  .find input::-webkit-search-cancel-button {
    display: none;
  }

  mark {
    border-radius: 2px;
    background: var(--accentDim);
    color: var(--accent);
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
    overflow-wrap: anywhere;
  }

  /* A line too long for the pane goes on under its own text rather than under the numbers, broken
     where it has a space, and anywhere in a word too long for a line. */
  .line {
    display: block;
    padding-left: 7ch;
    text-indent: -7ch;
  }

  .n {
    display: inline-block;
    min-width: 5ch;
    margin-right: 2ch;
    color: var(--faint);
    text-align: right;
    text-indent: 0;
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
