<script lang="ts">
  import { refusal, type API, type Me, type Namespace } from "../api/client";
  import type { components } from "../api/schema";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import { explain, type Explained } from "../lib/problem";
  import BranchProtection from "./BranchProtection.svelte";
  import Icon from "./Icon.svelte";
  import Notice from "./Notice.svelte";
  import Pane from "./Pane.svelte";

  // A workflow's settings, in sections one under another, each under the permission the API asks of
  // it and drawn only to a caller who holds it: its name, under workflow:write; its default branch
  // and protection, under grant:manage; a move to another namespace, under workflow:write and the
  // ownership of both namespaces; and last, deleting it with its runs, under workflow:delete.
  let {
    api,
    place,
    me,
    namespace,
    workflow,
    repository,
    namespaces,
  }: {
    api: API;
    place: Place;
    me: Me;
    namespace: string;
    workflow: string;
    repository: components["schemas"]["workflowDetail"]["repository"];
    namespaces: Namespace[];
  } = $props();

  const writes = $derived(holds(me, "workflow:write", namespace, workflow));
  const shares = $derived(holds(me, "grant:manage", namespace, workflow));
  const deletes = $derived(holds(me, "workflow:delete", namespace, workflow));
  // A workflow moves between namespaces its mover owns on both sides, which grant:manage in each
  // stands for here: the API decides, and says so where the console offered more than it takes.
  const owns = (ns: string) => holds(me, "grant:manage", ns);
  const targets = $derived(writes && owns(namespace) ? namespaces.filter((n) => n.name !== namespace && owns(n.name)).map((n) => n.name) : []);

  let working = $state(false);
  let said = $state("");
  let problem = $state<Explained | null>(null);

  async function act(failed: string, work: () => Promise<void>) {
    if (working) return;
    working = true;
    problem = null;
    said = "";
    try {
      await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // The rename: the name typed, sent as the one field PATCH changes, then the workflow opened under
  // its new name, its settings still.
  let name = $state("");
  $effect(() => {
    name = workflow;
  });

  function rename(e: SubmitEvent) {
    e.preventDefault();
    const to = name.trim();
    if (to === workflow) return;
    return act(`rename ${workflow}`, async () => {
      const { data, error, response } = await api.PATCH("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } }, body: { name: to } });
      if (!data) throw refusal(response, error);
      said = `Renamed to ${data.name}.`;
      place.go({ kind: "namespace", namespace, view: "workflows", workflow: data.name, tab: "settings" });
    });
  }

  // The move: the API answers at once and the leading controller carries it out, the workflow frozen
  // until it has, so the screen says where it goes and links to it there.
  let target = $state("");
  let moving = $state<string | null>(null);

  function move(e: SubmitEvent) {
    e.preventDefault();
    const to = target;
    if (!to) return;
    return act(`move ${workflow} to ${to}`, async () => {
      const answer = await api.PATCH("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } }, body: { namespace: to } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      moving = to;
    });
  }

  const moved = $derived(moving ? { kind: "namespace" as const, namespace: moving, view: "workflows" as const, workflow } : undefined);

  // Deleting takes the workflow's name typed again, since it takes every run with it and nothing
  // brings them back, which a second click is too easy to give.
  let confirm = $state("");

  function remove(e: SubmitEvent) {
    e.preventDefault();
    if (confirm.trim() !== workflow) return;
    return act(`delete ${workflow}`, async () => {
      const answer = await api.DELETE("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      place.go({ kind: "namespace", namespace, view: "workflows" });
    });
  }
</script>

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<div class="sections">
  <Pane title="General">
    <div class="general">
      <div class="field">
        <span class="label">Name</span>
        {#if writes}
          <form class="inline" onsubmit={rename} aria-label="Rename the workflow">
            <input class="term" bind:value={name} aria-label="Name" required pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" spellcheck="false" autocomplete="off" />
            <button class="control" disabled={working || name.trim() === workflow}>Rename</button>
          </form>
        {:else}
          <span class="term">{workflow}</span>
        {/if}
      </div>
      <div class="field">
        <span class="label">Clone</span>
        <span class="term clone">{repository.clone_url}</span>
      </div>
    </div>
  </Pane>

  {#if shares}<BranchProtection {api} {namespace} {workflow} />{/if}

  {#if targets.length > 0}
    <Pane title="Move">
      {#if moving && moved}
        <p class="moving">
          <span>Moving to <span class="term">{moving}</span>.</span>
          <a class="control" href={place.href(moved)} onclick={follow(place, moved)}>Open it there</a>
        </p>
      {:else}
        <div class="field">
          <span class="label">Namespace</span>
          <form class="inline" onsubmit={move} aria-label="Move the workflow">
            <select bind:value={target} aria-label="To the namespace" required>
              <option value="" disabled>To the namespace</option>
              {#each targets as t (t)}<option value={t}>{t}</option>{/each}
            </select>
            <button class="control" disabled={working || !target}>Move</button>
          </form>
        </div>
      {/if}
    </Pane>
  {/if}

  {#if deletes}
    <Pane title="Delete workflow" label="Delete workflow">
      <div class="field">
        <span class="label">Confirm</span>
        <form class="inline" onsubmit={remove} aria-label="Delete the workflow">
          <input class="term" bind:value={confirm} aria-label="The workflow's name, typed again" placeholder={workflow} spellcheck="false" autocomplete="off" />
          <button class="control danger" disabled={working || confirm.trim() !== workflow}><Icon name="control-remove" size={14} />Delete</button>
        </form>
      </div>
    </Pane>
  {/if}
</div>

<style>
  /* One column the width of the page, as a namespace's settings are. */
  .sections {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: calc(var(--unit) * 8);
  }

  .general {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  .field {
    display: grid;
    grid-template-columns: 120px minmax(0, 1fr);
    align-items: center;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .label {
    color: var(--muted);
  }

  .clone {
    overflow-wrap: anywhere;
  }

  .inline {
    display: flex;
    gap: calc(var(--unit) * 3);
    max-width: 420px;
  }

  .inline input,
  .inline select {
    flex: 1;
    min-width: 0;
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .moving {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin: 0;
    font-size: var(--type-control-size);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  @media (max-width: 759px) {
    .field {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
