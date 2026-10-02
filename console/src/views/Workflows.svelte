<script lang="ts">
  import { explain, refused as refusedHere, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import { untrack } from "svelte";
  import { refusal, type API, type Me } from "../api/client";
  import type { components } from "../api/schema";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import { clock } from "../lib/format";
  import { useLive } from "../lib/live.svelte";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import { runAt } from "../lib/route";

  // A namespace's workflows, and a new one created. They are GET /api/v1/{ns}/workflows, every one
  // whose runs the caller reads, each with its newest run, one never run among them, and nothing of
  // its file, so that an operator finds here what it may run. Creating one is POST
  // /api/v1/{ns}/workflows under workflow:write at the namespace's scope: an empty repository, its
  // default branch born by the first push, which the workflow's Files then say how to make.
  let { api, place, me, namespace }: { api: API; place: Place; me: Me; namespace: string } = $props();

  type Listed = components["schemas"]["listedWorkflow"];
  let known = $state<Listed[] | null>(null);
  let refused = $state<Explained | null>(null);
  let reading = false;

  async function read() {
    reading = true;
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows", { params: { path: { ns: namespace } } });
    reading = false;
    if (!data) {
      refused = explain("load the workflows", refusal(response, error));
      return;
    }
    refused = null;
    known = data.workflows;
  }
  $effect(() => {
    untrack(() => read());
  });

  // Read again each time the live connection says a run of the namespace changed, since the newest
  // run of each workflow is what the page shows.
  const changes = useLive();
  $effect(() =>
    changes.when((c) => c.kind === "run" && c.namespace === namespace, () => {
      if (!reading) read();
    }),
  );

  const mayCreate = $derived(holds(me, "workflow:write", namespace));
  let creating = $state(false);
  let name = $state("");
  let branch = $state("main");
  let protect = $state(false);
  let sending = $state(false);
  let said = $state<Explained | null>(null);

  async function create(e: SubmitEvent) {
    e.preventDefault();
    sending = true;
    said = null;
    const body = { name: name.trim(), ...(branch.trim() && branch.trim() !== "main" ? { default_branch: branch.trim() } : {}), ...(protect ? { protected: true } : {}) };
    const { data, error, response } = await api.POST("/api/v1/{ns}/workflows", { params: { path: { ns: namespace } }, body });
    sending = false;
    if (!data) {
      said = response.status === 409 ? refusedHere(`create ${body.name}`, `${body.name} is already taken.`) : explain(`create ${body.name}`, refusal(response, error));
      return;
    }
    place.go({ kind: "namespace", namespace, view: "workflows", workflow: data.name, tab: "files" });
  }

  const now = Date.now();
</script>

<PageHeader title="Workflows" icon="control-workflows" count={known?.length} {place}>
  {#snippet actions()}
    {#if mayCreate}<button class="control primary" onclick={() => ((creating = true), (said = null))}><Icon name="control-add" size={14} />New workflow</button>{/if}
  {/snippet}
</PageHeader>

<Pane title="" label="Workflows of {namespace}">
  {#if refused}
    <Problem explained={refused} onretry={read} />
  {:else if !known}
    <p class="muted" role="status">Loading</p>
  {:else}
    <table>
      <thead><tr><th>Workflow</th><th>Latest run</th><th>Run at</th><th>Created</th></tr></thead>
      <tbody>
        {#each known as k (k.name)}
          {@const page = { kind: "namespace" as const, namespace, view: "workflows" as const, workflow: k.name }}
          <tr>
            <td><a class="term" href={place.href(page)} onclick={follow(place, page)}>{k.name}</a></td>
            {#if k.latest}
              {@const run = runAt(namespace, k.name, k.latest.run)}
              <td><StatePill state={k.latest.state} /> <a class="code faint" href={place.href(run)} onclick={follow(place, run)}>{k.latest.run}</a></td>
              <td class="term"><time datetime={k.latest.created_at}>{clock(k.latest.created_at, now)}</time></td>
            {:else}
              <td class="muted">Never run</td>
              <td></td>
            {/if}
            <td class="term muted"><time datetime={k.created_at}>{clock(k.created_at, now)}</time></td>
          </tr>
        {:else}
          <tr><td colspan="4" class="muted">No workflows</td></tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Pane>

{#if mayCreate}
  <Dialog title="New workflow in {namespace}" bind:open={creating}>
    <form onsubmit={create} aria-label="New workflow">
      <label for="new-name"><span>Name</span></label>
      <input id="new-name" class="term" bind:value={name} required autocomplete="off" />
      <label for="new-branch"><span>Default branch</span></label>
      <input id="new-branch" class="term" bind:value={branch} autocomplete="off" />
      <label class="check"><input type="checkbox" bind:checked={protect} /><span>Protected</span></label>
      {#if said}<Problem explained={said} />{/if}
      <button class="control primary" type="submit" disabled={sending || !name.trim()}>Create</button>
    </form>
  </Dialog>
{/if}

<style>

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th,
  td {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    text-align: left;
    white-space: nowrap;
  }

  th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  form {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 3);
    font-size: var(--type-control-size);
  }

  label {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin-top: calc(var(--unit) * 3);
  }

  label.check {
    flex-direction: row;
    align-items: baseline;
    gap: calc(var(--unit) * 3);
  }

  input:not([type="checkbox"]) {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--sunken);
    color: var(--text);
  }

  form .control {
    align-self: flex-start;
    margin-top: calc(var(--unit) * 4);
  }

</style>
