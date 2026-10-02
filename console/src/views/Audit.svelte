<script lang="ts">
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import AdminTabs from "../components/AdminTabs.svelte";
  import Pane from "../components/Pane.svelte";
  import Problem from "../components/Problem.svelte";
  import { clock } from "../lib/format";
  import type { Place } from "../lib/place.svelte";
  import { explain, type Explained } from "../lib/problem";

  // The audit log, to an administrator: the newest entries first, a page at a time, each on one line
  // that opens on its detail as it was recorded. The four narrowings are matched exactly by the
  // route, so they are sent as typed, and the address keeps them, so that a narrowed log is a link.
  // An entry past the last one the chain was proved to is marked until a term proves it.
  let { api, place }: { api: API; place: Place } = $props();

  type Entry = components["schemas"]["auditPage"]["entries"][number];
  const fields = ["actor", "action", "namespace", "target"] as const;
  type Field = (typeof fields)[number];
  const label: Record<Field, string> = { actor: "Actor", action: "Action", namespace: "Namespace", target: "Target" };

  const asked = $derived(Object.fromEntries(fields.map((f) => [f, place.query.get(f) ?? ""])) as Record<Field, string>);

  let entries = $state<Entry[]>([]);
  let head = $state(0);
  let verified = $state(0);
  let more = $state(false);
  let loaded = $state(false);
  let working = $state(false);
  let unread = $state<Explained | null>(null);
  let open = $state<number | null>(null);
  const now = Date.now();
  const page = 50;

  let typed = $state<Record<Field, string>>({ actor: "", action: "", namespace: "", target: "" });

  async function read(narrowed: Record<Field, string>, before?: number) {
    working = true;
    try {
      const query: Record<string, string | number> = { limit: page };
      for (const f of fields) if (narrowed[f]) query[f] = narrowed[f];
      if (before) query.before = before;
      const { data, error, response } = await api.GET("/api/v1/auth/audit", { params: { query } });
      if (!data) throw refusal(response, error);
      entries = before ? [...entries, ...data.entries] : data.entries;
      head = data.head;
      verified = data.verified;
      more = data.entries.length === page;
      unread = null;
    } catch (e) {
      unread = explain("load the audit log", e);
    } finally {
      working = false;
      loaded = true;
    }
  }

  $effect(() => {
    const narrowed = asked;
    typed = { ...narrowed };
    open = null;
    read(narrowed);
  });

  function narrow(e: SubmitEvent) {
    e.preventDefault();
    const q = new URLSearchParams(place.query);
    for (const f of fields) {
      const v = typed[f].trim();
      if (v) q.set(f, v);
      else q.delete(f);
    }
    place.narrow(q);
  }

  function clear() {
    const q = new URLSearchParams(place.query);
    for (const f of fields) q.delete(f);
    place.narrow(q);
  }

  const narrowed = $derived(fields.some((f) => asked[f]));

  // detailOf is an entry's detail laid out, the text of a JSON object as recorded, or as written
  // where it is not one.
  function detailOf(e: Entry): string {
    try {
      return JSON.stringify(JSON.parse(e.detail), null, 2);
    } catch {
      return e.detail;
    }
  }
</script>

<AdminTabs {place} current="audit" />

<form class="narrow" onsubmit={narrow} aria-label="Narrow the audit log">
  {#each fields as f (f)}
    <label>
      <span>{label[f]}</span>
      <input class="term" bind:value={typed[f]} placeholder={f === "namespace" ? "- for the installation" : ""} autocomplete="off" spellcheck="false" />
    </label>
  {/each}
  <span class="buttons">
    <button class="control primary" disabled={working}>Narrow</button>
    {#if narrowed}<button class="control" type="button" onclick={clear}>Clear</button>{/if}
  </span>
</form>

<Pane title="Audit log" aside={head ? `${head} entries` : ""}>
  {#if unread && entries.length === 0}
    <Problem explained={unread} onretry={() => read(asked)} />
  {:else if !loaded}
    <p class="muted">Loading</p>
  {:else if entries.length === 0}
    <p class="muted">No entries</p>
  {:else}
    <table>
      <thead><tr><th>Seq</th><th>When</th><th>Actor</th><th>Action</th><th>Namespace</th><th>Target</th><th>Result</th></tr></thead>
      <tbody>
        {#each entries as e (e.seq)}
          <tr class:opened={open === e.seq}>
            <td class="term seq" class:unproved={e.seq > verified}>
              <button class="name" aria-expanded={open === e.seq} onclick={() => (open = open === e.seq ? null : e.seq)} title={e.seq > verified ? "Not yet proved" : undefined}>{e.seq}</button>
            </td>
            <td class="muted"><time datetime={e.at} title={e.at}>{clock(e.at, now)}</time></td>
            <td class="term">{e.actor}</td>
            <td class="term">{e.action}</td>
            <td class="term">{#if e.namespace}{e.namespace}{:else}<span class="faint">installation</span>{/if}</td>
            <td class="term">{e.target}</td>
            <td class:muted={e.result === "unchanged"}>{e.result}</td>
          </tr>
          {#if open === e.seq}
            <tr class="detail">
              <td colspan="7">
                <pre class="term">{detailOf(e)}</pre>
                <p class="hash term faint">{e.hash}</p>
              </td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
    {#if unread}<Problem explained={unread} onretry={() => read(asked, entries.at(-1)?.seq)} />{/if}
    {#if more}
      <p class="more"><button class="control" disabled={working} onclick={() => read(asked, entries.at(-1)?.seq)}>Older</button></p>
    {/if}
  {/if}
</Pane>

<style>
  .narrow {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    gap: calc(var(--unit) * 4);
    margin: 0 0 calc(var(--unit) * 8);
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 1);
    font-size: var(--type-control-size);
  }

  label > span {
    color: var(--muted);
  }

  input {
    width: 180px;
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .buttons {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
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

  /* One line an entry: a narrow screen scrolls the table rather than folding an identifier. */
  td {
    padding: calc(var(--unit) * 3);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    vertical-align: middle;
    white-space: nowrap;
  }

  tr.opened td {
    background: var(--accentDim);
  }

  .name {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: inherit;
    font-family: inherit;
    cursor: pointer;
  }

  /* An entry the chain has not been proved to yet: its number in the waiting colour. */
  .unproved .name {
    color: var(--running);
  }

  .detail td {
    background: var(--sunken);
    white-space: normal;
  }

  pre {
    margin: 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .hash {
    margin: calc(var(--unit) * 2) 0 0;
    overflow-wrap: anywhere;
  }

  .more {
    margin: calc(var(--unit) * 6) 0 0;
    text-align: center;
  }

  @media (max-width: 759px) {
    input {
      width: 100%;
    }

    label {
      flex: 1 1 140px;
    }
  }
</style>
