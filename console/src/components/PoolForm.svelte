<script lang="ts">
  import { refusal, type API, type Namespace } from "../api/client";
  import { labelPattern, labelsOf, type Pool } from "../lib/fleet";
  import { explain, type Explained } from "../lib/problem";
  import Icon from "./Icon.svelte";
  import Problem from "./Problem.svelte";

  // A new runner pool, with POST /api/v1/runner-pools: its name, the labels its runners may claim, the
  // namespaces it accepts, every one where none is ticked, and the most one task may ask for on it.
  // Its containment is hardened, the one tier a pool is created with (#registering-a-runner).
  let { api, namespaces, oncreated }: { api: API; namespaces: Namespace[]; oncreated: (pool: Pool) => void } = $props();

  let name = $state("");
  let labels = $state("");
  let accepted = $state<string[]>([]);
  let cpu = $state("");
  let memory = $state("");
  let pids = $state("");
  let working = $state(false);
  let problem = $state<Explained | null>(null);

  const written = $derived(labelsOf(labels));
  const wrong = $derived(written.filter((l) => !labelPattern.test(l)));

  async function create(e: SubmitEvent) {
    e.preventDefault();
    if (wrong.length) return;
    working = true;
    problem = null;
    const resource_ceilings = { ...(cpu.trim() ? { cpu: cpu.trim() } : {}), ...(memory.trim() ? { memory: memory.trim() } : {}), ...(pids.trim() ? { pids: Number(pids) } : {}) };
    const pool: Pool = { name: name.trim(), labels: written, namespaces: accepted, resource_ceilings, containment: "hardened" };
    const { data, response, error } = await api.POST("/api/v1/runner-pools", { body: { pool } });
    working = false;
    if (!data) {
      problem = explain("create the pool", refusal(response, error));
      return;
    }
    oncreated(data.pool);
  }
</script>

{#if problem}<Problem explained={problem} />{/if}
<form onsubmit={create} aria-label="New pool">
  <label>
    <span>Name</span>
    <input class="term" bind:value={name} placeholder="dmz" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
  </label>
  <label>
    <span>Labels</span>
    <input class="term" bind:value={labels} placeholder="zone=dmz, arch=amd64" autocomplete="off" aria-invalid={wrong.length > 0} aria-describedby="pool-labels-wrong" />
  </label>
  {#if wrong.length}<span class="wrong" id="pool-labels-wrong">Not key=value: <span class="term">{wrong.join(", ")}</span></span>{/if}
  <fieldset>
    <legend>Accepts</legend>
    <div class="namespaces">
      {#each namespaces as ns (ns.name)}
        <label class="check"><input type="checkbox" value={ns.name} bind:group={accepted} /><span class="term">{ns.name}</span></label>
      {/each}
    </div>
    <span class="muted note">{accepted.length ? "Only those ticked" : "Every namespace"}</span>
  </fieldset>
  <fieldset>
    <legend>Ceilings a task</legend>
    <div class="ceilings">
      <label><span>CPU</span><input class="term" bind:value={cpu} placeholder="2" pattern="0*[1-9][0-9]*(\.[0-9]+)?|0*\.[0-9]*[1-9][0-9]*" inputmode="decimal" autocomplete="off" /></label>
      <label><span>Memory</span><input class="term" bind:value={memory} placeholder="2Gi" pattern="[1-9][0-9]*(Ki|Mi|Gi|Ti)" autocomplete="off" /></label>
      <label><span>Processes</span><input class="term" bind:value={pids} placeholder="256" type="number" min="1" step="1" autocomplete="off" /></label>
    </div>
  </fieldset>
  <p class="buttons"><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Create</button></p>
</form>

<style>
  form {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  label > span:first-child,
  legend {
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  fieldset {
    display: grid;
    gap: calc(var(--unit) * 2);
    margin: 0;
    padding: 0;
    border: none;
  }

  legend {
    margin-bottom: calc(var(--unit) * 2);
    padding: 0;
  }

  .namespaces {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 8);
  }

  label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .note {
    font-size: var(--type-identifier-size-min);
  }

  .ceilings {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: calc(var(--unit) * 5);
  }

  .wrong {
    color: var(--failed);
    font-size: var(--type-identifier-size-min);
  }

  .buttons {
    display: flex;
    justify-content: flex-end;
    margin: 0;
  }
</style>
