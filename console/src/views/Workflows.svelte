<script lang="ts">
  import { untrack } from "svelte";
  import { refusal, type API, type Me } from "../api/client";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import { clock } from "../lib/format";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";

  // A namespace's workflows, and a new one created. The API lists no namespace's workflows, so the
  // page lists those its runs name, each with its latest run, and says so rather than passing them
  // off as every one there is. Creating one is POST /api/v1/{ns}/workflows under workflow:write at
  // the namespace's scope: an empty repository, its default branch born by the first push, which
  // the workflow's Files then say how to make.
  let { api, place, me, namespace }: { api: API; place: Place; me: Me; namespace: string } = $props();

  type Known = { workflow: string; run: string; state: string; created_at: string };
  let known = $state<Known[] | null>(null);
  let refused = $state("");

  async function read() {
    const { data, error, response } = await api.GET("/api/v1/runs", { params: { query: { namespace, limit: 200 } } });
    if (!data) {
      refused = refusal(response, error).message;
      return;
    }
    const seen = new Map<string, Known>();
    for (const r of data.runs) {
      if (r.namespace === namespace && !seen.has(r.workflow)) seen.set(r.workflow, { workflow: r.workflow, run: r.run, state: r.state, created_at: r.created_at });
    }
    known = [...seen.values()].sort((a, b) => (a.workflow < b.workflow ? -1 : 1));
  }
  $effect(() => {
    untrack(() => read());
  });

  const mayCreate = $derived(holds(me, "workflow:write", namespace));
  let name = $state("");
  let branch = $state("main");
  let protect = $state(false);
  let sending = $state(false);
  let said = $state("");

  async function create(e: SubmitEvent) {
    e.preventDefault();
    sending = true;
    said = "";
    const body = { name: name.trim(), ...(branch.trim() && branch.trim() !== "main" ? { default_branch: branch.trim() } : {}), ...(protect ? { protected: true } : {}) };
    const { data, error, response } = await api.POST("/api/v1/{ns}/workflows", { params: { path: { ns: namespace } }, body });
    sending = false;
    if (!data) {
      said = response.status === 409 ? `A workflow named ${body.name} is already in ${namespace}, or one deleted under that name is still being purged.` : refusal(response, error).message;
      return;
    }
    place.go({ kind: "namespace", namespace, view: "workflows", workflow: data.name, tab: "files" });
  }

  const now = Date.now();
</script>

<div class="columns">
  <Pane title="Workflows" aside="those the runs of {namespace} name">
    {#if refused}
      <p class="refused" role="alert">The runs could not be read: {refused}</p>
    {:else if !known}
      <p class="muted" role="status">Reading the runs of {namespace}.</p>
    {:else}
      <p class="muted note">The API lists no namespace's workflows, so these are the ones a run among the last 200 names. A workflow nothing has run is reached by its address, or created here.</p>
      <table>
        <thead><tr><th>Workflow</th><th>Latest run</th><th>Created</th></tr></thead>
        <tbody>
          {#each known as k (k.workflow)}
            {@const page = { kind: "namespace" as const, namespace, view: "workflows" as const, workflow: k.workflow }}
            {@const run = { kind: "namespace" as const, namespace, view: "runs" as const, run: k.run }}
            <tr>
              <td><a class="mono" href={place.href(page)} onclick={follow(place, page)}>{k.workflow}</a></td>
              <td><StatePill state={k.state} /> <a class="mono faint" href={place.href(run)} onclick={follow(place, run)}>{k.run}</a></td>
              <td class="mono"><time datetime={k.created_at}>{clock(k.created_at, now)}</time></td>
            </tr>
          {:else}
            <tr><td colspan="3" class="muted">No run of {namespace} yet.</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </Pane>

  {#if mayCreate}
    <Pane title="New workflow" aside="an empty repository in {namespace}">
      <form onsubmit={create}>
        <label for="new-name"><span>Name</span><span class="muted">what agentiik.yaml's metadata.name will write</span></label>
        <input id="new-name" class="mono" bind:value={name} required autocomplete="off" />
        <label for="new-branch"><span>Default branch</span><span class="muted">the one a run naming no ref runs, born by the first push</span></label>
        <input id="new-branch" class="mono" bind:value={branch} autocomplete="off" />
        <label class="check"><input type="checkbox" bind:checked={protect} /><span>Protect it: a push to it then takes <span class="mono">grant:manage</span>, where <span class="mono">workflow:write</span> is enough otherwise</span></label>
        {#if said}<p class="refused" role="alert">{said}</p>{/if}
        <button class="control primary" type="submit" disabled={sending || !name.trim()}>Create {name.trim() || "the workflow"}</button>
      </form>
    </Pane>
  {/if}
</div>

<style>
  .columns {
    display: grid;
    grid-template-columns: minmax(0, 1.4fr) minmax(320px, 1fr);
    gap: calc(var(--unit) * 7);
    align-items: start;
  }

  .note {
    margin: 0 0 calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th,
  td {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
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
    height: 29px;
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

  .refused {
    color: var(--failed);
  }
</style>
