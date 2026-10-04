<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import type { API } from "../api/client";
  import { changeCollection, collectionsOf, makeCollection, mcpServed, offered, removeCollection, removeMember, serverAddress, type Collection } from "../lib/collections";
  import type { Place } from "../lib/place.svelte";
  import Icon from "./Icon.svelte";
  import Pane from "./Pane.svelte";
  import Notice from "./Notice.svelte";
  import Dialog from "./Dialog.svelte";

  // What the caller gives an MCP client: the user's server, one address for every principal, each
  // answered as themselves; and their collections, the connectors they assemble from the workflows
  // they may run, each one a tool, each with the address a client is given. A client presents an API
  // token at either. What a member offers is the API's to say, read at the head of its ref, and a
  // member offering nothing says why. Workflows are added from a workflow's MCP tab.
  let { api, place }: { api: API; place: Place } = $props();

  const server = $derived(serverAddress(place.baseURI));
  const served = mcpServed(document);

  let collections = $state<Collection[] | null>(null);
  let unread = $state<Explained | null>(null);

  async function reread() {
    try {
      collections = await collectionsOf(api);
      unread = null;
    } catch (e) {
      unread = explain("load your collections", e);
    }
  }

  $effect(() => {
    reread();
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
      const done = await work();
      await reread();
      said = done;
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // copied is the address last copied, which its button marks.
  let copied = $state("");
  async function copy(address: string) {
    await navigator.clipboard.writeText(address);
    copied = address;
  }

  let making = $state(false);
  let name = $state("");
  let description = $state("");

  function make(e: SubmitEvent) {
    e.preventDefault();
    return act("make the collection", async () => {
      const c = await makeCollection(api, name.trim(), description.trim());
      making = false;
      name = "";
      description = "";
      return `${c.name} made.`;
    });
  }

  // editing is the collection whose name and description are being written.
  let editing = $state<Collection | null>(null);
  let newName = $state("");
  let newDescription = $state("");

  function edit(c: Collection) {
    editing = c;
    newName = c.name;
    newDescription = c.description;
    problem = null;
  }

  function save(e: SubmitEvent) {
    e.preventDefault();
    const c = editing;
    if (!c) return;
    const change: { name?: string; description?: string } = {};
    if (newName.trim() !== c.name) change.name = newName.trim();
    if (newDescription.trim() !== c.description) change.description = newDescription.trim();
    if (!Object.keys(change).length) {
      editing = null;
      return;
    }
    return act("change the collection", async () => {
      const saved = await changeCollection(api, c.id, change);
      editing = null;
      return `${saved.name} saved.`;
    });
  }

  // asking is what waits on a second click: a collection's removal, or a member's.
  let asking = $state("");

  function remove(c: Collection) {
    return act("remove the collection", async () => {
      await removeCollection(api, c.id);
      asking = "";
      return `${c.name} removed.`;
    });
  }

  function takeOut(c: Collection, workflow: string) {
    return act("take the workflow out", async () => {
      await removeMember(api, c.id, workflow);
      asking = "";
      return `${workflow} taken out of ${c.name}.`;
    });
  }
</script>

{#if problem && !making && !editing}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<Pane title="Your server">
  {#if !served}<p class="muted off">This installation serves no MCP, as AGK_MCP is off: these addresses answer 404 until it does. Collections are kept and changed all the same.</p>{/if}
  <dl class="server">
    <dt>Address</dt>
    <dd>
      <span class="term">{server}</span>
      <button class="control" onclick={() => copy(server)}><Icon name={copied === server ? "state-succeeded" : "control-copy"} size={14} />Copy</button>
    </dd>
    <dt>Credential</dt>
    <dd><a href={place.href({ kind: "account", tab: "tokens" })} onclick={(e) => { e.preventDefault(); place.go({ kind: "account", tab: "tokens" }); }}>An API token</a></dd>
  </dl>
</Pane>

<Pane title="Collections" aside={collections ? String(collections.length) : ""}>
  {#snippet actions()}
    <button class="control primary" onclick={() => ((making = true), (problem = null))}><Icon name="control-add" size={14} />New collection</button>
  {/snippet}
  {#if unread}
    <Problem explained={unread} onretry={reread} />
  {:else if collections === null}
    <p class="muted">Loading</p>
  {:else}
    {#each collections as c (c.id)}
      <section class="collection" aria-label="Collection {c.name}">
        <header>
          <span class="term name">{c.name}</span>
          {#if c.description}<span class="muted">{c.description}</span>{/if}
          <span class="end">
            <button class="control" disabled={working} onclick={() => edit(c)}>Edit</button>
            {#if asking === c.id}
              <button class="control" onclick={() => (asking = "")}>Keep</button>
              <button class="control danger" disabled={working} onclick={() => remove(c)}>Remove</button>
            {:else}
              <button class="control" disabled={working} onclick={() => (asking = c.id)}>Remove</button>
            {/if}
          </span>
        </header>
        <p class="address">
          <span class="term">{c.url}</span>
          <button class="control" onclick={() => copy(c.url)}><Icon name={copied === c.url ? "state-succeeded" : "control-copy"} size={14} />Copy</button>
        </p>
        <table>
          <thead><tr><th>Workflow</th><th>Read at</th><th>Tool</th><th class="end"></th></tr></thead>
          <tbody>
            {#each c.members as m (m.workflow)}
              <tr>
                <td class="term">{m.workflow}</td>
                <td class="term muted">{m.ref ?? "default branch"}</td>
                <td class:term={!!m.tool} class:muted={!m.tool}>{offered(m)}</td>
                <td class="end">
                  {#if asking === `${c.id} ${m.workflow}`}
                    <button class="control" onclick={() => (asking = "")}>Keep</button>
                    <button class="control danger" disabled={working} onclick={() => takeOut(c, m.workflow)}>Take out</button>
                  {:else}
                    <button class="control" disabled={working} onclick={() => (asking = `${c.id} ${m.workflow}`)}>Take out</button>
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="4" class="muted">No workflows</td></tr>
            {/each}
          </tbody>
        </table>
      </section>
    {:else}
      <p class="muted">No collections</p>
    {/each}
  {/if}
</Pane>

<Dialog title="New collection" bind:open={making}>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={make} aria-label="New collection">
    <label><span>Name</span><input class="term" bind:value={name} required pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" autocomplete="off" placeholder="back-office" /></label>
    <label><span>Description</span><input bind:value={description} maxlength="280" autocomplete="off" /></label>
    <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Make it</button></p>
  </form>
</Dialog>

<Dialog title="Edit the collection" open={editing !== null} onclose={() => (editing = null)}>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={save} aria-label="Edit the collection">
    <label><span>Name</span><input class="term" bind:value={newName} required pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" autocomplete="off" /></label>
    <label><span>Description</span><input bind:value={newDescription} maxlength="280" autocomplete="off" /></label>
    <p><button class="control primary" disabled={working}>Save</button></p>
  </form>
</Dialog>

<style>
  .server {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    align-items: center;
    margin: 0;
    font-size: var(--type-control-size);
  }

  dt {
    color: var(--muted);
  }

  dd {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin: 0;
    overflow-wrap: anywhere;
  }

  .off {
    margin: 0 0 calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  .collection {
    padding: calc(var(--unit) * 6) 0;
    font-size: var(--type-control-size);
  }

  .collection + .collection {
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
  }

  .collection header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 5);
  }

  .name {
    font-weight: 600;
  }

  header .end {
    margin-left: auto;
  }

  .address {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin: calc(var(--unit) * 3) 0;
    overflow-wrap: anywhere;
  }

  table {
    width: 100%;
    border-collapse: collapse;
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
  }

  label > span:first-child {
    color: var(--muted);
  }

  input {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  form p {
    margin: 0;
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }
</style>
