<script lang="ts">
  import { tokens } from "../lib/envelope";
  import type { Graph } from "../lib/graph";

  // What a workflow publishes as tools, exactly as a client of /mcp/{namespace}/{workflow} sees it:
  // the endpoint, the server's name and description, and each tool's name, title, description,
  // mode, timeout and annotations, its inputSchema the schema of the workflow input it names as it
  // stands, and its outputSchema the schema of the output it names, where it names one carrying a
  // schema. Read off the version's own mcp block, which a push held together.
  let { graph, namespace, workflow }: { graph: Graph; namespace: string; workflow: string } = $props();

  type Tool = {
    name: string;
    title?: string;
    description: string;
    input: { from: { input: string } };
    output?: { from: { output: string } };
    mode?: "sync" | "async";
    timeout?: string;
    annotations?: Record<string, boolean>;
  };
  type Block = { name?: string; description?: string; tools: Tool[] };

  const block = $derived(graph.mcp as unknown as Block | undefined);
  const endpoint = $derived(new URL(`mcp/${encodeURIComponent(namespace)}/${encodeURIComponent(workflow)}`, document.baseURI).href);
  const inputs = $derived((graph.inputs ?? {}) as Record<string, { schema: unknown }>);
  const outputs = $derived((graph.outputs ?? {}) as Record<string, { schema?: unknown }>);
</script>

{#if block}
  <dl class="server">
    <dt>Endpoint</dt>
    <dd class="term">{endpoint}</dd>
    <dt>Name</dt>
    <dd class="term">{block.name ?? workflow}</dd>
    {#if block.description}
      <dt>Description</dt>
      <dd>{block.description}</dd>
    {/if}
  </dl>
  {#each block.tools as t (t.name)}
    {@const schema = inputs[t.input.from.input]?.schema}
    {@const out = t.output ? outputs[t.output.from.output]?.schema : undefined}
    <section class="tool" aria-label="Tool {t.name}">
      <header>
        <span class="term name">{t.name}</span>
        {#if t.title}<span>{t.title}</span>{/if}
        <span class="term muted">mode: {t.mode ?? "sync"}{t.timeout ? ` · timeout: ${t.timeout}` : ""}</span>
      </header>
      <p>{t.description}</p>
      {#if t.annotations && Object.keys(t.annotations).length}
        <p class="term faint">{Object.entries(t.annotations).map(([k, v]) => `${k}: ${v}`).join(" · ")}</p>
      {/if}
      <p class="label">inputSchema, the schema of the input <span class="term">{t.input.from.input}</span></p>
      <pre class="json"><code>{#each tokens(schema ?? true) as tok, i (i)}<span class="t-{tok.kind}">{tok.text}</span>{/each}</code></pre>
      {#if t.output}
        <p class="label">
          {#if out !== undefined}outputSchema, the schema of the output <span class="term">{t.output.from.output}</span>{:else}The output <span class="term">{t.output.from.output}</span> carries no schema, so none is published{/if}
        </p>
        {#if out !== undefined}<pre class="json"><code>{#each tokens(out) as tok, i (i)}<span class="t-{tok.kind}">{tok.text}</span>{/each}</code></pre>{/if}
      {/if}
    </section>
  {:else}
    <p class="muted">The block lists no tool, so the endpoint serves an empty list.</p>
  {/each}
{:else}
  <p class="muted">This version declares no mcp block, so the endpoint answers 404, as a route the installation does not serve.</p>
{/if}

<style>
  .server {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    margin: 0 0 calc(var(--unit) * 8);
    font-size: var(--type-control-size);
  }

  dt {
    color: var(--muted);
  }

  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }

  .tool {
    padding: calc(var(--unit) * 6) 0;
    border-top: var(--border-hairline) solid var(--line);
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
