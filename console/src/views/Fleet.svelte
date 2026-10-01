<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import { untrack } from "svelte";
  import { apiBase, refusal, type API, type Namespace } from "../api/client";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import JoinForm from "../components/JoinForm.svelte";
  import Notice from "../components/Notice.svelte";
  import Pane from "../components/Pane.svelte";
  import PoolForm from "../components/PoolForm.svelte";
  import Popover from "../components/Popover.svelte";
  import StatePill from "../components/StatePill.svelte";
  import { capacity, ceilings, condition, counted, offered, reachedBy, type Pool, type Runner } from "../lib/fleet";
  import { clock, took } from "../lib/format";
  import { moved, useKeys } from "../lib/keys.svelte";
  import { useLive } from "../lib/live.svelte";
  import { follow, type Place } from "../lib/place.svelte";

  // The installation's runners and their pools, an administrator's alone, from GET /api/v1/runners
  // and GET /api/v1/runner-pools: what a namespace's allowed_runner_pools is written against. A pool
  // says what it accepts, what it caps, what its runners offer now and which namespaces may send work
  // to it; a runner says its pool, labels, condition, concurrency, last heartbeat and what its host
  // has. Never the host itself: the API holds no name or address of one, and a user learns of a
  // runner its identifier alone.
  //
  // An administrator creates a pool here, adds a runner to one by issuing a join token, replaces a
  // runner to change its labels, and drains or revokes one with a reason (#registering-a-runner). A
  // runner is never edited or removed: the API checked its labels against the token they were claimed
  // with, so a change to them is a new runner, and a revoked one stays listed for the audit log.
  let { api, place, namespaces, version }: { api: API; place: Place; namespaces: Namespace[]; version: string } = $props();

  let pools = $state<Pool[] | null>(null);
  let runners = $state<Runner[] | null>(null);
  let refused = $state<Explained | null>(null);
  let now = $state(Date.now());

  async function read() {
    const [p, r] = await Promise.all([api.GET("/api/v1/runner-pools"), api.GET("/api/v1/runners")]);
    if (p.data) pools = p.data.runner_pools.map((x) => x.pool);
    if (r.data) runners = r.data.runners;
    refused = !p.data ? explain("load the runner pools", refusal(p.response, p.error)) : !r.data ? explain("load the runners", refusal(r.response, r.error)) : null;
    now = Date.now();
  }

  // Read again each time the live connection says a runner or a pool changed, a heartbeat among
  // them, so that a drain taking hold is seen without reloading the page; and the clock moved every
  // second, since a runner falling silent is said by nothing but time passing.
  const changes = useLive();
  $effect(() => {
    untrack(read);
    const reading = changes.when((c) => c.kind === "runners", read);
    const ticking = setInterval(() => (now = Date.now()), 1000);
    return () => {
      reading();
      clearInterval(ticking);
    };
  });

  // The pool the runners are narrowed to, named in the address as ?pool=.
  const chosen = $derived(place.query.get("pool") ?? undefined);
  const shown = $derived(runners?.filter((r) => chosen === undefined || r.pool === chosen) ?? []);

  // choose narrows the runners to a pool, and to every pool again where it is the one chosen.
  function choose(pool: string | undefined) {
    narrowTo(pool === chosen ? undefined : pool);
  }

  function narrowTo(pool: string | undefined) {
    const q = new URLSearchParams(place.query);
    if (pool === undefined) q.delete("pool");
    else q.set("pool", pool);
    place.narrow(q);
  }

  useKeys(() => [
    ...(pools?.length
      ? [{ keys: ["ArrowUp", "ArrowDown", "k", "j"], brief: ["ArrowUp", "ArrowDown"], effect: "Pool", does: (key: string) => narrowTo(moved(pools!.map((p) => p.name), chosen, key)) }]
      : []),
    ...(chosen ? [{ keys: ["Escape"], effect: "Every pool", does: () => narrowTo(undefined) }] : []),
  ]);

  // What is being asked: a new pool, a runner added or replaced, or an order given to a runner.
  let creating = $state(false);
  let adding = $state(false);
  let joinPool = $state("");
  let joinLabels = $state<string[]>([]);
  let replacing = $state<string | null>(null);
  let ordering = $state<{ runner: string; order: "drain" | "revoke" } | null>(null);
  let reason = $state("");
  let working = $state(false);
  let refusedOrder = $state<Explained | null>(null);
  let told = $state("");

  const base = $derived(apiBase(place.baseURI));

  function add(pool?: string, labels?: string[], runner?: string) {
    const first = pools?.find((p) => p.name === (pool ?? chosen)) ?? pools?.[0];
    joinPool = first?.name ?? "";
    joinLabels = labels ?? [...(first?.labels ?? [])];
    replacing = runner ?? null;
    adding = true;
  }

  function created(pool: Pool) {
    creating = false;
    told = `Pool ${pool.name} created.`;
    read();
  }

  function order(runner: string, what: "drain" | "revoke") {
    ordering = { runner, order: what };
    reason = "";
    refusedOrder = null;
  }

  async function give(e: SubmitEvent) {
    e.preventDefault();
    if (!ordering) return;
    working = true;
    refusedOrder = null;
    const { runner, order: what } = ordering;
    const path = what === "drain" ? "/api/v1/runners/{runner}/drain" : "/api/v1/runners/{runner}/revoke";
    const { data, response, error } = await api.POST(path, { params: { path: { runner } }, body: { reason: reason.trim() } });
    working = false;
    if (!data) {
      refusedOrder = explain(what === "drain" ? "drain the runner" : "revoke the runner", refusal(response, error));
      return;
    }
    ordering = null;
    told = `Runner ${runner} ${what === "drain" ? "drained" : "revoked"}.`;
    read();
  }

  const of = (pool: string) => runners?.filter((r) => r.pool === pool) ?? [];
  const statistics = { kind: "runners" as const, tab: "statistics" };
  // An agent another release than the installation's is one somebody left behind on a host; a
  // console built from no tag says nothing of it.
  const release = $derived(/^v\d/.test(version) ? version.slice(1) : undefined);
</script>

<!-- A list of namespaces, each kept whole on its line: a name breaks nowhere, its hyphens included. -->
{#snippet names(list: string[])}
  {#each list as n, i (n)}<span class="term nowrap">{n}{i < list.length - 1 ? "," : ""}</span>{i < list.length - 1 ? " " : ""}{/each}
{/snippet}

<PageHeader title="Runners" icon="control-runners" {place} tabs={[
  { label: "Runners and pools", icon: "control-runners", to: { kind: "runners" }, current: true },
  { label: "Statistics", icon: "control-statistics", to: { kind: "runners", tab: "statistics" }, current: false },
]}>
  {#snippet actions()}
    <button class="control" onclick={() => (creating = true)}><Icon name="control-add" size={14} />New pool</button>
    <button class="control primary" disabled={!pools?.length} onclick={() => add()}><Icon name="control-add" size={14} />Add a runner</button>
  {/snippet}
</PageHeader>

{#if told}{#key told}<Notice ondismiss={() => (told = "")}>{told}</Notice>{/key}{/if}

<Dialog title="New pool" bind:open={creating} width={560}>
  <PoolForm {api} {namespaces} oncreated={created} />
</Dialog>

<Dialog title={replacing ? "Replace a runner" : "Add a runner"} bind:open={adding} width={640} onclose={() => read()}>
  {#if pools}
    {#if replacing}<p class="replacing">Replaces <span class="term">{replacing}</span></p>{/if}
    <JoinForm {api} {base} {pools} bind:pool={joinPool} bind:ticked={joinLabels} replace={replacing !== null} ondone={() => ((adding = false), read())} />
  {/if}
</Dialog>

<Dialog title={ordering?.order === "revoke" ? "Revoke a runner" : "Drain a runner"} open={ordering !== null} onclose={() => (ordering = null)}>
  {#if ordering}
    {#if refusedOrder}<Problem explained={refusedOrder} />{/if}
    <form class="order-form" onsubmit={give} aria-label={ordering.order === "revoke" ? "Revoke a runner" : "Drain a runner"}>
      <p class="term">{ordering.runner}</p>
      <label>
        <span>Reason</span>
        <input bind:value={reason} required maxlength="256" pattern="[^\x00-\x1f\x7f-\x9f]+" placeholder="kernel update" autocomplete="off" />
      </label>
      <p class="buttons">
        {#if ordering.order === "revoke"}
          <button class="control danger" disabled={working}>Revoke</button>
        {:else}
          <button class="control primary" disabled={working}>Drain</button>
        {/if}
      </p>
    </form>
  {/if}
</Dialog>

{#if refused}
  <Problem explained={refused} onretry={read} />
{/if}

<!-- The runners' pane is drawn once the pools above it are read, so that it is not pushed down. -->
{#if pools === null && !refused}
  <p class="muted" role="status">Loading</p>
{:else}
<Pane title="Pools" aside={pools ? `${pools.length} ${pools.length === 1 ? "pool" : "pools"}` : ""}>
  {#if pools}
    <div class="scroll">
      <table>
        <thead>
          <tr><th>Pool</th><th>Labels</th><th>Accepts</th><th>Ceilings a task</th><th>Containment</th><th>Runners</th><th class="number">Takes at once</th><th>Reached by</th></tr>
        </thead>
        <tbody>
          {#each pools as p (p.name)}
            {@const reached = reachedBy(p, namespaces)}
            <tr class:chosen={chosen === p.name}>
              <td><button class="name term" aria-pressed={chosen === p.name} onclick={() => choose(p.name)}>{p.name}</button></td>
              <td class="labels">{#each p.labels as l (l)}<span class="chip term">{l}</span>{:else}<span class="muted">no label</span>{/each}</td>
              <td>{#if p.namespaces.length}{@render names(p.namespaces)}{:else}<span class="muted">all namespaces</span>{/if}</td>
              <td class="term nowrap">{ceilings(p)}</td>
              <td class="term">{p.containment}</td>
              <td class="nowrap">{of(p.name).length ? counted(of(p.name), now) : "none"}</td>
              <td class="number term">{offered(of(p.name), now)}</td>
              <td>{#if reached.length}{@render names(reached)}{:else}<span class="muted">no namespace</span>{/if}</td>
            </tr>
          {:else}
            <tr><td colspan="8" class="muted">No pool yet.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</Pane>

<div class="below">
  <Pane title="Runners" aside={runners ? `${shown.length} ${chosen ? `in ${chosen}` : `of ${runners.length}`}` : ""}>
    {#if chosen}
      <p class="narrowed">In <span class="term">{chosen}</span> alone. <button class="link" onclick={() => choose(undefined)}>Every pool</button></p>
    {/if}
    {#if runners === null}
      <p class="muted">Loading</p>
    {:else}
      <div class="scroll">
        <table>
          <thead>
            <tr><th>Runner</th><th>Pool</th><th>Labels</th><th>Condition</th><th class="number">Concurrency</th><th>Last heartbeat</th><th>Host has</th><th>Agent</th><th class="end"><span class="unseen">Orders</span></th></tr>
          </thead>
          <tbody>
            {#each shown as r (r.runner)}
              {@const c = condition(r, now)}
              <tr>
                <td class="term nowrap">{r.runner}</td>
                <td class="term">{r.pool}</td>
                <td class="labels">{#each r.labels as l (l)}<span class="chip term">{l}</span>{:else}<span class="muted">no label</span>{/each}</td>
                <td class="condition">
                  <span class="nowrap"><StatePill state={c} />{#if c !== r.reported_state && r.reported_state && r.state !== "revoked"}<span class="muted said">says {r.reported_state}</span>{/if}</span>
                  {#if r.revoked_by && r.revoked_at}
                    <span class="order">revoked by <span class="term">{r.revoked_by}</span>, <time datetime={r.revoked_at} title={r.revoked_at}>{clock(r.revoked_at, now)}</time>{#if r.drain_reason}: {r.drain_reason}{/if}</span>
                    {#if r.results_accepted_until}<span class="order muted">results accepted until <time datetime={r.results_accepted_until} title={r.results_accepted_until}>{clock(r.results_accepted_until, now)}</time></span>{/if}
                  {:else if r.drained_by && r.drained_at}
                    <span class="order">drained by <span class="term">{r.drained_by}</span>, <time datetime={r.drained_at} title={r.drained_at}>{clock(r.drained_at, now)}</time>{#if r.drain_reason}: {r.drain_reason}{/if}</span>
                  {/if}
                </td>
                <td class="number term">{r.concurrency ?? ""}</td>
                <td class="nowrap">
                  {#if r.last_seen_at}<time class="term" datetime={r.last_seen_at} title={r.last_seen_at}>{took(Math.max(0, now - Date.parse(r.last_seen_at)))} ago</time>{:else}<span class="muted">not yet</span>{/if}
                </td>
                <td class="term nowrap">{capacity(r)}</td>
                <td class="term" class:behind={release !== undefined && r.agent_version !== release} title={release !== undefined && r.agent_version !== release ? `The installation runs ${release}` : undefined}>{r.agent_version}</td>
                <td class="end">
                  {#if r.state !== "revoked"}
                    <Popover label="Orders to {r.runner}" align="end" width={200}>
                      {#snippet button()}<span class="more"><Icon name="control-more" size={16} /></span>{/snippet}
                      {#snippet children(close)}
                        <div class="orders">
                          <button class="item" onclick={() => (close(), add(r.pool, [...r.labels], r.runner))}>Replace</button>
                          {#if r.state !== "draining"}<button class="item" onclick={() => (close(), order(r.runner, "drain"))}>Drain</button>{/if}
                          <button class="item danger" onclick={() => (close(), order(r.runner, "revoke"))}>Revoke</button>
                        </div>
                      {/snippet}
                    </Popover>
                  {/if}
                </td>
              </tr>
            {:else}
              <tr><td colspan="9" class="muted">{chosen ? `No runner has joined ${chosen}.` : "No runner has joined yet."}</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </Pane>
</div>
{/if}

<style>


  .below {
    margin-top: calc(var(--unit) * 12);
  }

  .scroll {
    overflow-x: auto;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th,
  td {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    text-align: left;
    vertical-align: middle;
  }

  th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  tr.chosen td {
    background: var(--accentDim);
  }

  .number {
    text-align: right;
  }

  .nowrap {
    white-space: nowrap;
  }

  .name {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: inherit;
    cursor: pointer;
  }

  .chip {
    display: inline-block;
    margin: 1px calc(var(--unit) * 2) 1px 0;
    padding: 1px calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    font-size: var(--type-identifier-size-min);
    white-space: nowrap;
  }

  .said {
    margin-left: calc(var(--unit) * 3);
    font-size: var(--type-identifier-size-min);
  }

  .condition {
    min-width: 200px;
  }

  .order {
    display: block;
    margin-top: calc(var(--unit) * 1);
    font-size: var(--type-identifier-size-min);
  }

  .behind {
    color: var(--waiting);
  }

  .narrowed {
    margin: 0 0 calc(var(--unit) * 3);
    font-size: var(--type-control-size);
  }

  .end {
    width: 1%;
    text-align: right;
  }

  .more {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    border-radius: var(--radius-control);
    color: var(--muted);
  }

  .more:hover {
    background: var(--raised);
    color: var(--text);
  }

  .orders {
    display: flex;
    flex-direction: column;
  }

  .item {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: none;
    border-radius: var(--radius-control);
    background: none;
    color: var(--text);
    font-size: var(--type-control-size);
    text-align: left;
    cursor: pointer;
  }

  .item:hover {
    background: var(--surface);
  }

  .item.danger {
    color: var(--failed);
  }

  .replacing {
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .order-form {
    display: grid;
    gap: calc(var(--unit) * 5);
  }

  .order-form p {
    margin: 0;
  }

  .order-form label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  .order-form label > span {
    color: var(--muted);
  }

  .order-form .buttons {
    display: flex;
    justify-content: flex-end;
  }

  .link {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent);
    font-size: inherit;
    cursor: pointer;
  }
</style>
