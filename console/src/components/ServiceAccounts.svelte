<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { refusal, type API, type Me } from "../api/client";
  import type { ServiceAccount } from "../lib/credentials";
  import { clock } from "../lib/format";
  import { sentence } from "../lib/signin";
  import Icon from "./Icon.svelte";
  import Pane from "./Pane.svelte";
  import Notice from "./Notice.svelte";

  // The service accounts of the namespaces the caller owns: non-human principals, written NS/NAME,
  // that hold API tokens and never sign in to a client. Creating one gives nobody anything, since it
  // holds no grant and no token until one is written or minted for it; removing one takes its tokens
  // and grants with it. The built-in NS/agentiik, which a namespace's scheduled, webhook and event runs
  // are attributed to, goes only with its namespace.
  let { api, me }: { api: API; me: Me } = $props();

  let accounts = $state<ServiceAccount[] | null>(null);
  let unread = $state<Explained | null>(null);

  // Every one, the built-in NS/agentiik of each namespace among them, which the tokens' list leaves
  // out since none is minted for it.
  async function reread() {
    const { data, error, response } = await api.GET("/api/v1/service-accounts");
    if (data) {
      accounts = data.service_accounts;
      unread = null;
    } else unread = explain("load the service accounts", refusal(response, error));
  }

  $effect(() => {
    reread();
  });

  // The namespaces a service account may be created in: those where the caller holds grant:manage on
  // the namespace itself, which the owner role carries. The API decides on ownership; this only
  // keeps the choice to the namespaces it could say yes to.
  const owned = $derived(Object.entries(me.permissions).filter(([scope, held]) => !scope.includes("/") && held.includes("grant:manage")).map(([scope]) => scope).sort());

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

  let namespace = $state("");
  let name = $state("");
  $effect(() => {
    if (!namespace && owned.length) namespace = owned[0]!;
  });

  function create(e: SubmitEvent) {
    e.preventDefault();
    return act("create the service account", async () => {
      const { data, error, response } = await api.POST("/api/v1/service-accounts", { body: { namespace, name: name.trim() } });
      if (!data) throw refusal(response, error);
      name = "";
      return `${data.namespace}/${data.name} is created. It holds no grant and no token until one is written or minted for it.`;
    });
  }

  // asking is the service account whose removal waits on a second click.
  let asking = $state("");
  const id = (a: ServiceAccount) => `${a.namespace}/${a.name}`;

  function remove(a: ServiceAccount) {
    return act("remove the service account", async () => {
      const answer = await api.DELETE("/api/v1/service-accounts/{ns}/{name}", { params: { path: { ns: a.namespace, name: a.name } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      asking = "";
      return `${id(a)} is removed, with its tokens and its grants.`;
    });
  }

  const now = Date.now();
</script>

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<div class="columns">
  <Pane title="Service accounts" aside={accounts ? String(accounts.length) : ""}>
    {#if unread}
      <Problem explained={unread} onretry={reread} />
    {:else if accounts === null}
      <p class="muted">Reading the service accounts.</p>
    {:else}
      <table>
        <thead><tr><th>Service account</th><th>Created</th><th class="end"></th></tr></thead>
        <tbody>
          {#each accounts as a (id(a))}
            <tr>
              <td class="code">{id(a)}</td>
              <td class="muted">
                {#if a.name === "agentiik"}built in: its namespace's scheduled, webhook and event runs are attributed to it{:else if a.created_by}by <span class="term">{a.created_by}</span>{#if a.created_at}, <time datetime={a.created_at} title={a.created_at}>{clock(a.created_at, now)}</time>{/if}{/if}
              </td>
              <td class="end">
                {#if a.name !== "agentiik"}
                  {#if asking === id(a)}
                    <button class="control danger" disabled={working} onclick={() => remove(a)}>Remove {id(a)}</button>
                    <button class="control" onclick={() => (asking = "")}>Keep</button>
                  {:else}
                    <button class="control" disabled={working} onclick={() => (asking = id(a))}>Remove</button>
                  {/if}
                {/if}
              </td>
            </tr>
          {:else}
            <tr><td colspan="3" class="muted">You own no namespace, so you hold no service account.</td></tr>
          {/each}
        </tbody>
      </table>
      <p class="foot muted">To create a token for one of them, go to API tokens and choose the service account.</p>
    {/if}
  </Pane>

  <Pane title="Create a service account">
    {#if owned.length}
      <form onsubmit={create} aria-label="Create a service account">
        <label>
          <span>In</span>
          <select bind:value={namespace}>
            {#each owned as ns (ns)}<option value={ns}>{ns}</option>{/each}
          </select>
        </label>
        <label>
          <span>Name</span>
          <input class="term" bind:value={name} placeholder="deploy-bot" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
        </label>
        <p class="foot muted">Its full name is <span class="term">{namespace || "NS"}/{name.trim() || "NAME"}</span>. It can do nothing until you share something with it.</p>
        <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Create it</button></p>
      </form>
    {:else}
      <p class="muted">You can create a service account only in a namespace you own, and you own none.</p>
    {/if}
  </Pane>
</div>

<style>

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(300px, 1fr);
    gap: calc(var(--unit) * 8);
    align-items: start;
  }

  /* The form goes under the list where the two side by side would squeeze the list's columns. */
  @media (max-width: 1099px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
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

  input,
  select {
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

  .foot {
    margin: calc(var(--unit) * 6) 0 0;
    font-size: var(--type-control-size);
  }

  form .foot {
    margin: 0;
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }
</style>
