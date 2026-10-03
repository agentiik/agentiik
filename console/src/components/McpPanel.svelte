<script lang="ts">
  import type { API } from "../api/client";
  import { addMember, collectionsOf, mcpServed, offered, type Collection } from "../lib/collections";
  import { tokens } from "../lib/envelope";
  import type { Graph } from "../lib/graph";
  import { explain, type Explained } from "../lib/problem";
  import { toolOf } from "../lib/tool";
  import Problem from "./Problem.svelte";

  // The tool a workflow is, exactly as a client of a collection holding it sees it: its name,
  // title, description, mode, timeout and annotations, its inputSchema an object of the
  // workflow's inputs, each with its schema as it stands, and its outputSchema the schema of the
  // output it returns, where that output carries one. Read off the version's own mcp block, which
  // a push held together. Beside it, the caller's collections, those holding the workflow named
  // with what it offers there, and adding it to another a click away, which the API judges.
  let { api, graph, namespace, workflow }: { api: API; graph: Graph; namespace: string; workflow: string } = $props();

  const tool = $derived(toolOf(graph, workflow));
  const named = $derived(`${namespace}/${workflow}`);
  const served = mcpServed(document);

  let collections = $state<Collection[] | null>(null);
  let problem = $state<Explained | null>(null);
  let adding = $state("");

  async function reread() {
    try {
      collections = await collectionsOf(api);
    } catch (e) {
      problem = explain("load your collections", e);
    }
  }

  $effect(() => {
    reread();
  });

  async function add(c: Collection) {
    if (adding) return;
    adding = c.id;
    problem = null;
    try {
      const now = await addMember(api, c.id, named);
      collections = (collections ?? []).map((x) => (x.id === now.id ? now : x));
    } catch (e) {
      problem = explain(`add ${workflow} to ${c.name}`, e);
    } finally {
      adding = "";
    }
  }
</script>

{#if !served}<p class="muted off">This installation serves no MCP, as AGK_MCP is off: the mcp block is checked at every push and no client is offered the tool.</p>{/if}
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

<section class="collections" aria-label="Your collections">
  <p class="label">Your collections</p>
  {#if problem}<Problem explained={problem} />{/if}
  {#if collections === null}
    <p class="muted">Loading</p>
  {:else}
    <table>
      <tbody>
        {#each collections as c (c.id)}
          {@const member = c.members.find((m) => m.workflow === named)}
          <tr>
            <td class="term">{c.name}</td>
            <td class:term={!!member?.tool} class:muted={!member?.tool}>{member ? offered(member) : ""}</td>
            <td class="end">
              {#if !member}<button class="control" disabled={adding !== ""} onclick={() => add(c)}>Add</button>{/if}
            </td>
          </tr>
        {:else}
          <tr><td class="muted">No collections</td></tr>
        {/each}
      </tbody>
    </table>
  {/if}
</section>

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

  .tool p,
  .collections > p {
    margin: calc(var(--unit) * 3) 0;
  }

  .label {
    color: var(--muted);
  }

  .off {
    margin: 0 0 calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  .collections {
    margin-top: calc(var(--unit) * 8);
    font-size: var(--type-control-size);
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  td {
    padding: calc(var(--unit) * 3);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    vertical-align: middle;
  }

  .end {
    text-align: right;
    white-space: nowrap;
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
