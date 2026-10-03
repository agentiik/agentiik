<script lang="ts">
  import { tokens } from "../lib/envelope";
  import type { Graph } from "../lib/graph";
  import { toolOf } from "../lib/tool";

  // The tool a workflow is, exactly as a client of a collection holding it sees it: its name,
  // title, description, mode, timeout and annotations, its inputSchema an object of the
  // workflow's inputs, each with its schema as it stands, and its outputSchema the schema of the
  // output it returns, where that output carries one. Read off the version's own mcp block, which
  // a push held together.
  let { graph, workflow }: { graph: Graph; workflow: string } = $props();

  const tool = $derived(toolOf(graph, workflow));
</script>

{#if tool}
  <section class="tool" aria-label="Tool {tool.name}">
    <header>
      <span class="term name">{tool.name}</span>
      {#if tool.title}<span>{tool.title}</span>{/if}
      <span class="term muted">mode: {tool.mode}{tool.timeout ? ` · timeout: ${tool.timeout}` : ""}</span>
    </header>
    <p>{tool.description}</p>
    {#if Object.keys(tool.annotations).length}
      <p class="term faint">{Object.entries(tool.annotations).map(([k, v]) => `${k}: ${v}`).join(" · ")}</p>
    {/if}
    <p class="label">inputSchema</p>
    <pre class="json"><code>{#each tokens(tool.inputSchema) as tok, i (i)}<span class="t-{tok.kind}">{tok.text}</span>{/each}</code></pre>
    {#if tool.output}
      <p class="label">
        {#if tool.outputSchema !== undefined}outputSchema, the schema of the output <span class="term">{tool.output}</span>{:else}The output <span class="term">{tool.output}</span>, no schema{/if}
      </p>
      {#if tool.outputSchema !== undefined}<pre class="json"><code>{#each tokens(tool.outputSchema) as tok, i (i)}<span class="t-{tok.kind}">{tok.text}</span>{/each}</code></pre>{/if}
    {/if}
  </section>
{:else}
  <p class="muted">No tool</p>
{/if}

<style>
  .tool {
    font-size: var(--type-control-size);
  }

  .tool header {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: calc(var(--unit) * 5);
  }

  .name {
    font-size: var(--type-identifier-size-max);
    font-weight: 600;
  }

  .tool p {
    margin: calc(var(--unit) * 3) 0;
  }

  .label {
    color: var(--muted);
  }

  .json {
    max-height: 260px;
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
</style>
