<script lang="ts">
  import { explain, refused, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { refusal, type API, type Me } from "../api/client";
  import { sentence } from "../lib/signin";
  import { columns, expiryOf, kindOf, permissions, principalsOf, resolve, roles, type Grant, type Kind, type Line, type Role, type Scope } from "../lib/sharing";
  import Icon from "./Icon.svelte";
  import Pane from "./Pane.svelte";
  import Notice from "./Notice.svelte";

  // The sharing panel of a namespace or of one workflow of it: "who holds what, where each
  // permission comes from, what one person can actually do. One control adds, expires or denies."
  // The grants are listed as written, those a workflow inherits from its namespace said to be;
  // resolved for one principal they come to its permissions there, each with the grants that give
  // it and the deny that takes it away; and the roles' table says what each grant's role carries.
  // Listing, writing and revoking are grant:manage's at the scope, which the caller holds to be
  // shown the panel at all.
  let { api, me, namespace, workflow }: { api: API; me: Me; namespace: string; workflow?: string } = $props();

  const at = $derived<Scope>({ namespace, workflow });
  const here = $derived(workflow ? `${namespace}/${workflow}` : namespace);
  const now = Date.now();

  let grants = $state<Grant[] | null>(null);
  let unread = $state<Explained | null>(null);
  // The groups and their members, which an administrator alone reads: who else is in a group is
  // nobody else's to learn, so for anybody else a group's grants are said to apply to its members.
  let groups = $state<{ name: string; members: string[] }[] | null>(null);

  async function read() {
    unread = null;
    const answer = workflow
      ? await api.GET("/api/v1/{ns}/workflows/{name}/grants", { params: { path: { ns: namespace, name: workflow } } })
      : await api.GET("/api/v1/{ns}/grants", { params: { path: { ns: namespace } } });
    if (answer.data) {
      grants = answer.data.grants;
    } else {
      unread = explain("load the grants", refusal(answer.response, answer.error));
    }
  }

  $effect(() => {
    void here;
    read();
  });

  $effect(() => {
    if (!me.admin) return;
    api.GET("/api/v1/groups").then(({ data }) => {
      if (data) groups = data.groups.map((g) => ({ name: g.name, members: g.members }));
    });
  });

  // What is being done, said, and what the API refused, said as it said it.
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

  // The one control: whom, what, and until when.
  let kind = $state<Kind>("user");
  let name = $state("");
  let gives = $state<string>("role:viewer");
  let until = $state("");

  const principal = $derived(kind === "group" ? `group:${name.trim()}` : name.trim());
  const isDeny = $derived(gives.startsWith("deny:"));
  const what = $derived(gives.slice(gives.indexOf(":") + 1));

  function add(e: SubmitEvent) {
    e.preventDefault();
    const expires = expiryOf(until, Date.now());
    if (expires instanceof Error) {
      problem = refused("add the grant", sentence(`The expiry is ${expires.message}`));
      return;
    }
    return act("add the grant", async () => {
      const body = { principal, ...(isDeny ? { deny: what as Grant["deny"] } : { role: what as Role }), ...(expires ? { expires_at: expires } : {}) };
      const answer = workflow
        ? await api.POST("/api/v1/{ns}/workflows/{name}/grants", { params: { path: { ns: namespace, name: workflow } }, body })
        : await api.POST("/api/v1/{ns}/grants", { params: { path: { ns: namespace } }, body });
      if (!answer.data) throw refusal(answer.response, answer.error);
      said = isDeny ? `${what} denied to ${principal}.` : `${what} granted to ${principal}.`;
      name = "";
      until = "";
      await read();
    });
  }

  // asking is the grant whose revocation waits on a second click, so that one click revokes nothing.
  let asking = $state("");

  function revoke(g: Grant) {
    return act("revoke the grant", async () => {
      const answer = workflow
        ? await api.DELETE("/api/v1/{ns}/workflows/{name}/grants/{id}", { params: { path: { ns: namespace, name: workflow, id: g.id } } })
        : await api.DELETE("/api/v1/{ns}/grants/{id}", { params: { path: { ns: namespace, id: g.id } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      said = `${g.deny ? `Deny of ${g.deny}` : `${g.role} grant`} revoked for ${g.principal}.`;
      asking = "";
      await read();
    });
  }

  // Whom the arithmetic is worked for: the caller first, then everybody the grants name.
  const offered = $derived([...new Set([me.principal, ...(grants ? principalsOf(grants) : [])])]);
  let chosen = $state("");
  const whom = $derived(chosen && offered.includes(chosen) ? chosen : me.principal);

  // The groups of whom, where the caller can read them: its own, from GET /api/v1/me, or anybody's
  // for an administrator.
  const whoseGroups = $derived.by((): string[] | undefined => {
    // A group's grants are its own, and a service account belongs to no group.
    if (kindOf(whom) !== "user") return [];
    if (whom === me.principal) return me.groups.map((g) => g.replace(/^group:/, ""));
    if (groups) return groups.filter((g) => g.members.includes(whom)).map((g) => g.name);
    return undefined;
  });

  const lines = $derived<Line[]>(grants ? resolve({ ref: whom, groups: whoseGroups }, grants, at, now) : []);

  function source(g: Grant): string {
    const by = g.principal === whom ? "own" : g.principal;
    return `${g.role ?? `deny ${g.deny}`} at ${g.scope}${by === "own" ? "" : `, through ${by}`}`;
  }

  // day is an instant as a date, with its time where it is not midnight UTC, where an expiry
  // written as a number of days or a date falls.
  function day(instant: string): string {
    const written = new Date(instant).toISOString();
    return written.endsWith("T00:00:00.000Z") ? written.slice(0, 10) : written.slice(0, 16).replace("T", " ");
  }

  function inherited(g: Grant): boolean {
    return workflow !== undefined && g.scope === namespace;
  }

  const kinds: Kind[] = ["user", "group", "service account"];
  const placeholders = $derived<Record<Kind, string>>({ user: "login", group: "group name", "service account": `${namespace}/name` });
</script>

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<!-- Drawn once the grants are read, every pane at once, so that none is pushed down as the grants
     above it arrive. -->
{#if grants === null && !unread}
  <p class="muted" role="status">Loading</p>
{:else}
<div class="columns">
  <div class="stack">
    <Pane title="Grants" aside={grants ? `${grants.length} on ${here}` : here}>
      {#if unread}
        <Problem explained={unread} onretry={read} />
      {:else if grants !== null}
        <div class="scroll">
          <table>
            <thead><tr><th>Principal</th><th>Gives</th><th>Scope</th><th>Expires</th><th>Granted</th><th class="end"></th></tr></thead>
            <tbody>
              {#each grants as g (g.id)}
                <tr class:deny={g.deny !== undefined}>
                  <td><span class="kind muted">{kindOf(g.principal)}</span> <span class="term nowrap">{g.principal.replace(/^group:/, "")}</span></td>
                  <td>
                    {#if g.deny}<span class="chip deny term">deny {g.deny}</span>{:else}<span class="chip term">{g.role}</span>{/if}
                  </td>
                  <td class="term nowrap">{g.scope}{#if inherited(g)}<span class="muted inherited">inherited</span>{/if}</td>
                  <td class="term muted nowrap">{#if g.expires_at}<time datetime={g.expires_at} title={g.expires_at}>{day(g.expires_at)}</time>{:else}never{/if}</td>
                  <td class="muted nowrap"><span class="term">{g.granted_by}</span> <time class="term" datetime={g.granted_at} title={g.granted_at}>{g.granted_at.slice(0, 10)}</time></td>
                  <td class="end">
                    {#if inherited(g)}
                      <span class="muted">on {namespace}</span>
                    {:else if asking === g.id}
                      <span class="confirm">
                        <button class="control" onclick={() => (asking = "")}>Keep</button>
                        <button class="control danger" disabled={working} onclick={() => revoke(g)}><Icon name="control-remove" size={14} />Revoke</button>
                      </span>
                    {:else}
                      <button class="control" onclick={() => (asking = g.id)}><Icon name="control-remove" size={14} />Revoke</button>
                    {/if}
                  </td>
                </tr>
              {:else}
                <tr><td colspan="6" class="muted">No grants</td></tr>
              {/each}
            </tbody>
          </table>
        </div>

        <form class="add" onsubmit={add} aria-label="Add a grant or a deny">
          <label>
            <span>Principal</span>
            <span class="pair">
              <select bind:value={kind} aria-label="What the principal is">
                {#each kinds as k (k)}<option value={k}>{k}</option>{/each}
              </select>
              <input class="term" bind:value={name} placeholder={placeholders[kind]} aria-label="Who" required />
            </span>
          </label>
          <label>
            <span>Gives</span>
            <select bind:value={gives}>
              <optgroup label="A role">
                {#each roles as r (r.role)}<option value="role:{r.role}">{r.role}</option>{/each}
              </optgroup>
              <optgroup label="A deny, which takes one permission away">
                {#each permissions as p (p)}<option value="deny:{p}">deny {p}</option>{/each}
              </optgroup>
            </select>
          </label>
          <label>
            <span>Expires</span>
            <input class="term" bind:value={until} placeholder="never, 30d or 2027-01-01" />
          </label>
          <button class="control primary" disabled={working || name.trim() === ""}>Add</button>
        </form>
      {/if}
    </Pane>

    <Pane title="Roles">
      <table class="roles">
        <thead>
          <tr><th>Role</th>{#each columns as c (c.name)}<th class="mark" title={c.permissions.join(", ")}>{c.name}</th>{/each}</tr>
        </thead>
        <tbody>
          {#each roles as r (r.role)}
            <tr>
              <td class="term">{r.role}</td>
              {#each columns as c (c.name)}
                {@const yes = r.columns.includes(c.name)}
                <td class="mark" class:yes class:lack={r.role === "operator" && c.name === "read"}>{yes ? "yes" : "no"}</td>
              {/each}
            </tr>
          {/each}
        </tbody>
      </table>
    </Pane>
  </div>

  <div class="stack">
    <Pane title="Effective permissions" aside={here}>
      <label class="whom">
        <span class="unseen">Whom</span>
        <select value={whom} onchange={(e) => (chosen = e.currentTarget.value)} aria-label="Whom to resolve">
          {#each offered as p (p)}<option value={p}>{p}{p === me.principal ? " (you)" : ""}</option>{/each}
        </select>
      </label>
      {#if grants}
        <ul class="lines" aria-label="Effective permissions of {whom} on {here}">
          {#each lines as l (l.permission)}
            <li class:held={l.held} class:taken={l.takes.length > 0}>
              <span class="sign term" aria-hidden="true">{l.takes.length > 0 ? "−" : l.held ? "+" : " "}</span>
              <span class="term">{l.permission}</span>
              <span class="from muted">
                {#if l.takes.length > 0}
                  denied by {l.takes.map(source).join("; ")}
                {:else if l.held}
                  given by {l.gives.map(source).join("; ")}
                {:else if l.ifMember.length > 0}
                  if member of {l.ifMember.map((g) => g.principal.replace(/^group:/, "")).join(", ")}
                {:else}
                  not given
                {/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </Pane>

  </div>
</div>
{/if}

<style>
  .columns {
    display: grid;
    grid-template-columns: minmax(0, 3fr) minmax(320px, 2fr);
    gap: calc(var(--unit) * 8);
    align-items: start;
  }

  /* The grants and the arithmetic side by side where both fit, the one above the other in a
     window of 1280px, the narrowest the console is drawn for, where a grant's row needs the width. */
  @media (max-width: 1599px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  .scroll {
    overflow-x: auto;
  }

  .stack {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: calc(var(--unit) * 8);
    min-width: 0;
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

  td :global(svg) {
    margin-right: calc(var(--unit) * 2);
    vertical-align: -2px;
  }

  .nowrap {
    white-space: nowrap;
  }

  .inherited {
    display: block;
    font-size: var(--type-identifier-size-min);
  }

  .kind {
    font-size: var(--type-identifier-size-min);
  }

  .chip {
    display: inline-block;
    white-space: nowrap;
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-chip);
    background: var(--accentDim);
    color: var(--accent);
    line-height: 20px;
  }

  .chip.deny {
    border-color: var(--failed);
    background: transparent;
    color: var(--failed);
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  .confirm {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  .add {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(0, 1.3fr) minmax(0, 1.2fr) max-content;
    gap: calc(var(--unit) * 4);
    align-items: end;
    margin-top: calc(var(--unit) * 6);
    padding-top: calc(var(--unit) * 6);
    border-top: var(--border-hairline) solid var(--line);
  }

  .add label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  .add label > span:first-child {
    color: var(--muted);
  }

  .pair {
    display: flex;
    gap: calc(var(--unit) * 2);
  }

  .pair input {
    flex: 1;
    min-width: 0;
  }

  .pair select {
    flex: none;
  }

  .add input,
  .add select {
    width: 100%;
    min-width: 0;
  }

  .add .pair select {
    width: auto;
  }

  input,
  select {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .roles th.mark,
  .roles td.mark {
    text-align: center;
  }

  .roles td.mark {
    color: var(--faint);
  }

  .roles td.mark.yes {
    color: var(--text);
  }

  .roles td.lack {
    box-shadow: inset 0 0 0 1.5px var(--waiting);
    color: var(--waiting);
    font-weight: 600;
  }

  .whom select {
    width: 100%;
  }

  .lines {
    margin: calc(var(--unit) * 4) 0 0;
    padding: 0;
    list-style: none;
    font-size: var(--type-control-size);
  }

  .lines li {
    display: grid;
    grid-template-columns: 14px max-content 1fr;
    gap: calc(var(--unit) * 4);
    padding: calc(var(--unit) * 2) 0;
    border-top: var(--border-hairline) solid var(--line);
    color: var(--faint);
  }

  .lines li.held {
    color: var(--text);
  }

  .lines li.held .sign {
    color: var(--succeeded);
  }

  .lines li.taken {
    color: var(--failed);
  }

  .from {
    overflow-wrap: anywhere;
  }

  /* On a phone the fields of a grant go one above the other. */
  @media (max-width: 759px) {
    .add {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
