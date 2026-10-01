<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import type { API } from "../api/client";
  import { filesOf, readEnvelope, readOutput, shown, tokens, type Envelope, type Side } from "../lib/envelope";
  import { bytes } from "../lib/stats";
  import Icon from "./Icon.svelte";

  // One envelope, drawn for a principal who holds run:read_data on the run's workflow: its count and
  // when it was produced, its items as coloured JSON on the sunken ground, the first ones at once and
  // more on asking, and the files those items carry by name, media type, size and digest. Copy and
  // Download take the whole envelope as the API answered it, never only what is drawn. Given an
  // output, it is that workflow output of the run, which names the step and the port it is a view of.
  let {
    api,
    run,
    step,
    port,
    side,
    task,
    output,
  }: { api: API; run: string; step: string; port: string; side: Side; task?: { attempt: number; shard?: { index: number } }; output?: string } = $props();

  // How many items are drawn at once, and how many more each ask draws.
  const batch = 20;

  let envelope = $state<Envelope | null>(null);
  let refused = $state<Explained | null>(null);
  let drawn = $state(batch);
  let copied = $state(false);

  $effect(() => {
    const asked = { run, step, port, side, attempt: task?.attempt, shard: task?.shard?.index, output };
    envelope = null;
    refused = null;
    drawn = batch;
    (asked.output ? readOutput(api, asked.run, asked.output) : readEnvelope(api, asked.run, asked.step, asked.port, asked.side, task)).then(
      (e) => {
        envelope = e;
      },
      (e: unknown) => {
        refused = explain(asked.output ? `load the output ${asked.output}` : `load what the port ${asked.port} held`, e);
      },
    );
  });

  const view = $derived(envelope ? shown(envelope, drawn) : null);
  const coloured = $derived(view ? tokens(view.items) : []);
  const files = $derived(view ? filesOf(view.items) : []);

  async function copy() {
    if (!envelope) return;
    await navigator.clipboard.writeText(JSON.stringify(envelope, null, 2));
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }

  function download() {
    if (!envelope) return;
    const link = document.createElement("a");
    link.href = URL.createObjectURL(new Blob([JSON.stringify(envelope, null, 2)], { type: "application/json" }));
    link.download = output ? `${run}-output-${output}.json` : `${run}-${step}-${side}-${port}.json`;
    link.click();
    URL.revokeObjectURL(link.href);
  }
</script>

<section class="envelope" aria-label={output ? `The envelope of the output ${output}` : `The envelope on ${port}`}>
  {#if refused}
    <Problem explained={refused} />
  {:else if !envelope || !view}
    <p class="muted">Loading</p>
  {:else}
    <div class="head">
      <span class="term name">{output ?? port}</span>
      <span class="muted">{envelope.meta.count} {envelope.meta.count === 1 ? "item" : "items"} · produced <time class="term" datetime={envelope.meta.produced_at}>{envelope.meta.produced_at}</time></span>
      <span class="spacer"></span>
      <button class="control" onclick={copy}><Icon name="control-copy" size={14} />{copied ? "Copied" : "Copy"}</button>
      <button class="control" onclick={download}><Icon name="control-download" size={14} />Download</button>
    </div>
    {#if envelope.items.length === 0}
      <p class="muted">Empty</p>
    {:else}
      <pre class="json"><code>{#each coloured as t, i (i)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
      {#if drawn < envelope.items.length}
        <p>
          <button class="control" onclick={() => (drawn += batch)}>Show {Math.min(batch, envelope.items.length - drawn)} more items</button>
          <span class="muted">{drawn} of {envelope.items.length} drawn</span>
        </p>
      {/if}
      {#if files.length > 0}
        <table class="files">
          <thead><tr><th>File</th><th>Media type</th><th class="number">Size</th><th>SHA-256</th><th>Item</th></tr></thead>
          <tbody>
            {#each files as f (f.item + f.file.name)}
              <tr>
                <td class="term">{f.file.name}</td>
                <td class="term muted">{f.file.media_type}</td>
                <td class="number term">{bytes(f.file.size)}</td>
                <td class="code muted" title={f.file.sha256}>{f.file.sha256.slice(0, 12)}</td>
                <td class="term muted">{f.item}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    {/if}
  {/if}
</section>

<style>
  .envelope {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 4);
  }

  .head {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
  }

  .name {
    font-weight: 600;
  }

  .spacer {
    flex: 1;
  }

  .problem {
    color: var(--failed);
  }

  p {
    margin: 0;
  }

  .json {
    max-height: 480px;
    margin: 0;
    padding: calc(var(--unit) * 4) calc(var(--unit) * 5);
    overflow: auto;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-family: var(--type-identifier-font);
    font-size: 12px;
    --leading: 1.55;
  }

  .t-key {
    color: var(--accent);
  }

  .t-string {
    color: var(--waiting);
  }

  .t-number,
  .t-literal {
    color: var(--succeeded);
  }

  .files {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  .files th {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 3);
    color: var(--muted);
    font-weight: 500;
    text-align: left;
  }

  .files td {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 3);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
  }

  .files .number {
    text-align: right;
  }
</style>
