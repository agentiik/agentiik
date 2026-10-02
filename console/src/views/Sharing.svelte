<script lang="ts">
  import PageHeader from "../components/PageHeader.svelte";
  import type { API, Me } from "../api/client";
  import SharingPanel from "../components/SharingPanel.svelte";
  import { holds } from "../lib/permissions";
  import type { Place } from "../lib/place.svelte";

  // A namespace's sharing: the panel of the namespace, for a caller holding grant:manage there, and
  // that of one workflow of it, named in the address as ?workflow=, for a caller holding it on that
  // workflow. A workflow the caller may share is offered where GET /api/v1/me names it apart from its
  // namespace; any other is opened by its name, and one that is not there, or not the caller's to
  // share, is answered as the API answers it.
  let { api, place, me, namespace }: { api: API; place: Place; me: Me; namespace: string } = $props();

  const atNamespace = $derived(holds(me, "grant:manage", namespace));
  const managed = $derived(
    Object.entries(me.permissions)
      .filter(([scope, held]) => scope.startsWith(`${namespace}/`) && held.includes("grant:manage"))
      .map(([scope]) => scope.slice(namespace.length + 1))
      .sort(),
  );
  const named = $derived(place.query.get("workflow") ?? undefined);
  const workflow = $derived(named !== undefined && (atNamespace || managed.includes(named)) ? named : undefined);

  function open(wf: string | undefined) {
    const q = new URLSearchParams(place.query);
    if (wf) q.set("workflow", wf);
    else q.delete("workflow");
    place.narrow(q);
  }

  let typed = $state("");
</script>

<PageHeader title="Sharing" icon="control-share" {place} />

<div class="bar" role="group" aria-label="Scope">
  {#if atNamespace}
    <button class="chip term" aria-pressed={workflow === undefined} onclick={() => open(undefined)}>{namespace}</button>
  {/if}
  {#each managed as wf (wf)}
    <button class="chip term" aria-pressed={workflow === wf} onclick={() => open(wf)}>{namespace}/{wf}</button>
  {/each}
  {#if atNamespace}
    <form class="named" onsubmit={(e) => { e.preventDefault(); if (typed.trim()) open(typed.trim()); }}>
      <label class="unseen" for="sharing-workflow">A workflow of {namespace}</label>
      <input id="sharing-workflow" class="term" bind:value={typed} placeholder="a workflow of {namespace}" />
      <button class="control" disabled={typed.trim() === ""}>Open its grants</button>
    </form>
  {/if}
</div>

{#if atNamespace || workflow}
  {#key `${namespace}/${workflow ?? ""}`}
    <SharingPanel {api} {me} {namespace} {workflow} />
  {/key}
{/if}

<style>
  .bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    min-height: var(--bar-sub);
    margin-bottom: calc(var(--unit) * 6);
  }

  .chip {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 6);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-round);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .chip[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .named {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
    margin-left: auto;
  }

  input {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }
</style>
