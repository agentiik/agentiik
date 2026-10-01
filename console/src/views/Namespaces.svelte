<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { refusal, type API, type Namespace } from "../api/client";
  import AdminTabs from "../components/AdminTabs.svelte";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import Notice from "../components/Notice.svelte";
  import type { Pool } from "../lib/fleet";
  import type { Place } from "../lib/place.svelte";
  import { bodyOf, formOf, summary, units, type Form } from "../lib/quotas";
  import { sentence } from "../lib/signin";

  // The installation's namespaces, an administrator's: each with its kind, its owner and its quotas,
  // the quotas written here, a shared namespace created with the owner it starts with, and one that
  // holds nothing removed. The pools a namespace may send work to are chosen among those that exist,
  // each said to accept it or not, since a task is placed only where both sides agree.
  let { api, place, changed }: { api: API; place: Place; changed: () => Promise<void> } = $props();

  let namespaces = $state<Namespace[] | null>(null);
  let pools = $state<Pool[]>([]);
  let unread = $state<Explained | null>(null);

  async function reread() {
    const { data, error, response } = await api.GET("/api/v1/namespaces");
    if (data) {
      namespaces = data.namespaces;
      unread = null;
    } else unread = explain("load the namespaces", refusal(response, error));
  }

  $effect(() => {
    reread();
    api.GET("/api/v1/runner-pools").then(({ data }) => (pools = data?.runner_pools.map((p) => p.pool) ?? []));
  });

  let working = $state(false);
  let problem = $state<Explained | null>(null);
  let said = $state("");

  async function act(failed: string, work: () => Promise<string>) {
    if (working) return;
    working = true;
    problem = null;
    said = "";
    try {
      // Said once the namespaces are read again, so that what it says is what the page shows.
      const done = await work();
      await reread();
      await changed();
      said = done;
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // The namespace whose quotas are written, named in the address as ?namespace=.
  const chosen = $derived(place.query.get("namespace") ?? undefined);
  const record = $derived(namespaces?.find((n) => n.name === chosen));

  function choose(name: string | undefined) {
    const q = new URLSearchParams(place.query);
    if (name === undefined) q.delete("namespace");
    else q.set("namespace", name);
    place.narrow(q);
  }

  // The quota form, filled again from the record whenever another namespace is chosen.
  let form = $state<Form>(formOf(undefined));
  let wrong = $state<{ field: keyof Form; problem: string } | null>(null);
  $effect(() => {
    form = formOf(record?.quotas);
    wrong = null;
  });

  const accepts = (p: Pool, ns: string) => p.namespaces.length === 0 || p.namespaces.includes(ns);

  function toggle(pool: string, on: boolean) {
    form.allowed_runner_pools = on ? [...form.allowed_runner_pools, pool] : form.allowed_runner_pools.filter((p) => p !== pool);
  }

  function write(e: SubmitEvent) {
    e.preventDefault();
    if (!record) return;
    const read = bodyOf(form);
    if ("problem" in read) {
      wrong = read;
      return;
    }
    wrong = null;
    const ns = record.name;
    return act("save the quotas", async () => {
      const { data, error, response } = await api.PUT("/api/v1/namespaces/{ns}/quotas", { params: { path: { ns } }, body: read.body });
      if (!data) throw refusal(response, error);
      return "Quotas saved.";
    });
  }

  // The form that creates a shared namespace.
  let creating = $state(false);
  let name = $state("");
  let owner = $state("");

  function create(e: SubmitEvent) {
    e.preventDefault();
    return act("create the namespace", async () => {
      const { data, error, response } = await api.POST("/api/v1/namespaces", { body: { name: name.trim(), kind: "shared", owner: owner.trim() } });
      if (!data) throw refusal(response, error);
      creating = false;
      name = "";
      owner = "";
      return `${data.name} created.`;
    });
  }

  // asking is the namespace whose removal waits on a second click.
  let asking = $state("");

  function remove(n: Namespace) {
    return act("remove the namespace", async () => {
      const answer = await api.DELETE("/api/v1/namespaces/{ns}", { params: { path: { ns: n.name } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      asking = "";
      if (chosen === n.name) choose(undefined);
      return `${n.name} removed.`;
    });
  }

  const label: Record<keyof Form, string> = {
    max_concurrent_tasks: "Tasks at once",
    max_retention_days: "Days kept at most",
    max_runs_per_hour: "Runs an hour",
    max_artifact: "Artifacts kept",
    max_artifact_unit: "Unit",
    max_run_duration: "Longest run",
    allowed_runner_pools: "Pools it may send work to",
  };
</script>

<AdminTabs {place} current="namespaces">
  {#snippet actions()}
    <button class="control primary" onclick={() => ((creating = true), (problem = null))}><Icon name="control-add" size={14} />New namespace</button>
  {/snippet}
</AdminTabs>

{#if problem && !creating}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<Pane title="Namespaces" aside={namespaces ? String(namespaces.length) : ""}>
  {#if unread}
    <Problem explained={unread} onretry={reread} />
  {:else if namespaces === null}
    <p class="muted">Loading</p>
  {:else}
    <table>
      <thead><tr><th>Namespace</th><th>Kind</th><th>Owner</th><th>Quotas</th><th class="end"></th></tr></thead>
      <tbody>
        {#each namespaces as n (n.name)}
          <tr class:chosen={chosen === n.name}>
            <td><button class="name term" aria-pressed={chosen === n.name} onclick={() => choose(chosen === n.name ? undefined : n.name)}>{n.name}</button></td>
            <td class="muted">{n.kind}</td>
            <td class="term">{#if n.owner}{n.owner}{:else}<span class="muted">nobody named</span>{/if}</td>
            <td class="quotas">{summary(n.quotas)}</td>
            <td class="end">
              {#if n.kind === "shared"}
                {#if asking === n.name}
                  <button class="control" onclick={() => (asking = "")}>Keep</button>
                  <button class="control danger" disabled={working} onclick={() => remove(n)}>Remove</button>
                {:else}
                  <button class="control" disabled={working} onclick={() => (asking = n.name)}>Remove</button>
                {/if}
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Pane>

<Dialog title={record ? `Quotas of ${record.name}` : ""} open={record !== undefined} width={560} onclose={() => choose(undefined)}>
  {#if record}
    <form onsubmit={write} aria-label="Quotas of {record.name}" novalidate>
      {#each ["max_concurrent_tasks", "max_retention_days", "max_runs_per_hour"] as const as k (k)}
        <label>
          <span>{label[k]} <span class="term faint">{k}</span></span>
          <input class="term" inputmode="numeric" bind:value={form[k]} placeholder={k === "max_runs_per_hour" ? "no bound" : ""} aria-invalid={wrong?.field === k} />
        </label>
      {/each}
      <label>
        <span>{label.max_artifact} <span class="term faint">max_artifact_bytes</span></span>
        <span class="pair">
          <input class="term" inputmode="numeric" bind:value={form.max_artifact} placeholder="no bound" aria-invalid={wrong?.field === "max_artifact"} />
          <select bind:value={form.max_artifact_unit} aria-label="Unit of the artifacts kept">
            {#each units as u (u.unit)}<option value={u.unit}>{u.unit}</option>{/each}
          </select>
        </span>
      </label>
      <label>
        <span>{label.max_run_duration} <span class="term faint">max_run_duration</span></span>
        <input class="term" bind:value={form.max_run_duration} placeholder="no bound, such as 24h" aria-invalid={wrong?.field === "max_run_duration"} />
      </label>
      <fieldset>
        <legend>{label.allowed_runner_pools} <span class="term faint">allowed_runner_pools</span></legend>
        {#each pools as p (p.name)}
          <label class="check">
            <input type="checkbox" checked={form.allowed_runner_pools.includes(p.name)} onchange={(e) => toggle(p.name, e.currentTarget.checked)} />
            <span class="term">{p.name}</span>
            {#if !accepts(p, record.name)}<span class="muted">does not accept {record.name}</span>{/if}
          </label>
        {:else}
          <p class="muted">No pools</p>
        {/each}
      </fieldset>
      {#if wrong}<p class="problem" role="alert">{label[wrong.field]}: {wrong.problem}.</p>{/if}
      <p class="buttons">
        <button class="control primary" disabled={working}>Write the quotas</button>
        <button class="control" type="button" onclick={() => choose(undefined)}>Close</button>
      </p>
    </form>
  {/if}
</Dialog>

<Dialog title="New namespace" bind:open={creating}>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={create} aria-label="Create a namespace">
    <label>
      <span>Name</span>
      <input class="term" bind:value={name} placeholder="finance" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
    </label>
    <label>
      <span>Owner, a login or group:NAME</span>
      <input class="term" bind:value={owner} placeholder="group:finance-leads" required autocomplete="off" />
    </label>
    <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Create the namespace</button></p>
  </form>
</Dialog>

<style>
  .problem {
    margin: 0 0 calc(var(--unit) * 6);
    color: var(--failed);
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 3);
    color: var(--muted);
    font-weight: 500;
    text-align: left;
  }

  td {
    padding: calc(var(--unit) * 3);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    vertical-align: middle;
  }

  tr.chosen td {
    background: var(--accentDim);
  }

  .quotas {
    color: var(--muted);
  }

  .name {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: inherit;
    cursor: pointer;
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  .end .control + .control {
    margin-left: calc(var(--unit) * 2);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 5);
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  label > span:first-child {
    color: var(--muted);
  }

  label.check {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .pair {
    display: flex;
    gap: calc(var(--unit) * 2);
  }

  .pair input {
    flex: 1;
    min-width: 0;
  }

  input:not([type="checkbox"]),
  select {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  input[aria-invalid="true"] {
    border-color: var(--failed);
  }

  fieldset {
    display: grid;
    gap: calc(var(--unit) * 2);
    margin: 0;
    padding: 0;
    border: none;
    font-size: var(--type-control-size);
  }

  legend {
    margin-bottom: calc(var(--unit) * 2);
    color: var(--muted);
  }

  .faint {
    color: var(--faint);
    font-size: var(--type-identifier-size-min);
  }

  form p {
    margin: 0;
  }

  .buttons {
    display: flex;
    gap: calc(var(--unit) * 3);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }
</style>
