<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import AdminTabs from "../components/AdminTabs.svelte";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import Notice from "../components/Notice.svelte";
  import { listOf, usersOf } from "../lib/credentials";
  import type { Place } from "../lib/place.svelte";
  import { sentence } from "../lib/signin";

  // The installation's groups and who is in each, an administrator's alone. A group's members are
  // logins and never a group, and a group holds what grants give it, so that membership changes
  // without touching a grant: putting somebody in a group gives them what its grants give, from their
  // next request, and the owners of each namespace where that widens access are told.
  let { api, place }: { api: API; place: Place } = $props();

  type Group = components["schemas"]["groupList"]["groups"][number];

  let groups = $state<Group[] | null>(null);
  let logins = $state<string[]>([]);
  let unread = $state<Explained | null>(null);

  async function reread() {
    const { data, error, response } = await api.GET("/api/v1/groups");
    if (data) {
      groups = data.groups;
      unread = null;
    } else unread = explain("load the groups", refusal(response, error));
  }

  $effect(() => {
    reread();
    usersOf(api).then(
      (u) => (logins = u.map((x) => x.login)),
      () => (logins = []),
    );
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
      said = await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // The form that creates a group, with its first members.
  let creating = $state(false);
  let name = $state("");
  let first = $state("");

  function create(e: SubmitEvent) {
    e.preventDefault();
    return act("create the group", async () => {
      const members = listOf(first);
      const { data, error, response } = await api.POST("/api/v1/groups", { body: { name: name.trim(), ...(members.length ? { members } : {}) } });
      if (!data) throw refusal(response, error);
      creating = false;
      name = "";
      first = "";
      await reread();
      return `group:${data.name} created.`;
    });
  }

  // What is typed into each group's field that adds a member.
  let adding = $state<Record<string, string>>({});

  function join(g: Group) {
    const login = (adding[g.name] ?? "").trim();
    if (!login) return;
    return act("add the member", async () => {
      const { data, error, response } = await api.PUT("/api/v1/groups/{group}/members/{login}", { params: { path: { group: g.name, login } } });
      if (!data) throw refusal(response, error);
      adding[g.name] = "";
      await reread();
      return `${login} added.`;
    });
  }

  function leave(g: Group, login: string) {
    return act("remove the member", async () => {
      const { data, error, response } = await api.DELETE("/api/v1/groups/{group}/members/{login}", { params: { path: { group: g.name, login } } });
      if (!data) throw refusal(response, error);
      await reread();
      return `${login} removed.`;
    });
  }

  // asking is the group whose removal waits on a second click.
  let asking = $state("");

  function remove(g: Group) {
    return act("remove the group", async () => {
      const answer = await api.DELETE("/api/v1/groups/{group}", { params: { path: { group: g.name } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      asking = "";
      await reread();
      return `group:${g.name} removed.`;
    });
  }
</script>

<AdminTabs {place} current="groups">
  {#snippet actions()}
    <button class="control primary" onclick={() => ((creating = true), (problem = null))}><Icon name="control-add" size={14} />New group</button>
  {/snippet}
</AdminTabs>

{#if problem && !creating}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<datalist id="group-logins">
  {#each logins as l (l)}<option value={l}></option>{/each}
</datalist>

<Pane title="Groups" aside={groups ? String(groups.length) : ""}>
  {#if unread}
    <Problem explained={unread} onretry={reread} />
  {:else if groups === null}
    <p class="muted">Loading</p>
  {:else}
    <table>
      <thead><tr><th>Group</th><th>Members</th><th>Add a member</th><th class="end"></th></tr></thead>
      <tbody>
        {#each groups as g (g.name)}
          <tr>
            <td class="term nowrap">group:{g.name}</td>
            <td>
              {#each g.members as m (m)}
                <span class="chip term">{m}<button class="unchip" aria-label="Take {m} out of group:{g.name}" disabled={working} onclick={() => leave(g, m)}><Icon name="control-remove" size={12} /></button></span>
              {:else}
                <span class="muted">nobody</span>
              {/each}
            </td>
            <td>
              <form class="inline" onsubmit={(e) => { e.preventDefault(); join(g); }} aria-label="Add a member to group:{g.name}">
                <input class="term" list="group-logins" placeholder="a login" bind:value={adding[g.name]} aria-label="Login to add to group:{g.name}" />
                <button class="control" disabled={working}>Add</button>
              </form>
            </td>
            <td class="end">
              {#if asking === g.name}
                <button class="control" onclick={() => (asking = "")}>Keep</button>
                <button class="control danger" disabled={working} onclick={() => remove(g)}>Remove</button>
              {:else}
                <button class="control" disabled={working} onclick={() => (asking = g.name)}>Remove</button>
              {/if}
            </td>
          </tr>
        {:else}
          <tr><td colspan="4" class="muted">No groups</td></tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Pane>

<Dialog title="New group" bind:open={creating}>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={create} aria-label="Create a group">
    <label>
      <span>Name</span>
      <input class="term" bind:value={name} placeholder="team-finance" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
    </label>
    <label>
      <span>First members, by login</span>
      <input class="term" bind:value={first} placeholder="alice, bob" autocomplete="off" />
    </label>
    <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Create the group</button></p>
  </form>
</Dialog>

<style>

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

  .nowrap {
    white-space: nowrap;
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  .end .control + .control {
    margin-left: calc(var(--unit) * 2);
  }

  .chip {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 1);
    margin: 1px calc(var(--unit) * 2) 1px 0;
    padding: 1px calc(var(--unit) * 1) 1px calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    font-size: var(--type-identifier-size-min);
    white-space: nowrap;
  }

  .unchip {
    display: inline-flex;
    padding: 1px;
    border: none;
    background: none;
    color: var(--muted);
    cursor: pointer;
  }

  .unchip:hover {
    color: var(--failed);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 5);
  }

  form.inline {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
  }

  form.inline input {
    width: 9rem;
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
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
