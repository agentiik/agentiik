<script lang="ts">
  import PageHeader from "../components/PageHeader.svelte";
  import { untrack } from "svelte";
  import { refusal, type API, type Namespace } from "../api/client";
  import Pane from "../components/Pane.svelte";
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
  let { api, place, namespaces, version }: { api: API; place: Place; namespaces: Namespace[]; version: string } = $props();

  let pools = $state<Pool[] | null>(null);
  let runners = $state<Runner[] | null>(null);
  let refused = $state("");
  let now = $state(Date.now());

  async function read() {
    const [p, r] = await Promise.all([api.GET("/api/v1/runner-pools"), api.GET("/api/v1/runners")]);
    if (p.data) pools = p.data.runner_pools.map((x) => x.pool);
    if (r.data) runners = r.data.runners;
    refused = !p.data ? refusal(p.response, p.error).message : !r.data ? refusal(r.response, r.error).message : "";
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
]} />

{#if refused}
  <p class="refused" role="alert">The runners could not be read: {refused}</p>
{/if}

<!-- The runners' pane is drawn once the pools above it are read, so that it is not pushed down. -->
{#if pools === null && !refused}
  <p class="muted" role="status">Reading the pools.</p>
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
              <td class="nowrap">{#each p.labels as l (l)}<span class="chip term">{l}</span>{:else}<span class="muted">no label</span>{/each}</td>
              <td>{#if p.namespaces.length}{@render names(p.namespaces)}{:else}<span class="muted">every namespace</span>{/if}</td>
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
    <p class="muted note">A namespace reaches a pool where both agree: the pool accepts it, and its allowed_runner_pools lists the pool or lists nothing. Takes at once adds up the concurrency of the runners ready now.</p>
  {/if}
</Pane>

<div class="below">
  <Pane title="Runners" aside={runners ? `${shown.length} ${chosen ? `in ${chosen}` : `of ${runners.length}`}` : ""}>
    {#if chosen}
      <p class="narrowed">In <span class="term">{chosen}</span> alone. <button class="link" onclick={() => choose(undefined)}>Every pool</button></p>
    {/if}
    {#if runners === null}
      <p class="muted">Reading the runners.</p>
    {:else}
      <div class="scroll">
        <table>
          <thead>
            <tr><th>Runner</th><th>Pool</th><th>Labels</th><th>Condition</th><th class="number">Concurrency</th><th>Last heartbeat</th><th>Host has</th><th>Agent</th></tr>
          </thead>
          <tbody>
            {#each shown as r (r.runner)}
              {@const c = condition(r, now)}
              <tr>
                <td class="term nowrap">{r.runner}</td>
                <td class="term">{r.pool}</td>
                <td class="nowrap">{#each r.labels as l (l)}<span class="chip term">{l}</span>{:else}<span class="muted">no label</span>{/each}</td>
                <td class="condition">
                  <span class="nowrap"><StatePill state={c} />{#if c !== r.reported_state && r.reported_state && r.state !== "revoked"}<span class="muted said">says {r.reported_state}</span>{/if}</span>
                  {#if r.revoked_by && r.revoked_at}
                    <span class="order">revoked by <span class="term">{r.revoked_by}</span>, <time datetime={r.revoked_at} title={r.revoked_at}>{clock(r.revoked_at, now)}</time>{#if r.drain_reason}: {r.drain_reason}{/if}</span>
                    {#if r.results_accepted_until}<span class="order muted">its results taken until <time datetime={r.results_accepted_until} title={r.results_accepted_until}>{clock(r.results_accepted_until, now)}</time></span>{/if}
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
              </tr>
            {:else}
              <tr><td colspan="8" class="muted">{chosen ? `No runner has joined ${chosen}.` : "No runner has joined yet."}</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="muted note">A runner is its identifier, pool and labels here, never the host it runs on: GET /api/v1/runners names none. A runner silent for 30 s, three heartbeats missed, has had its tasks declared lost.</p>
    {/if}
  </Pane>
</div>
{/if}

<style>

  .refused {
    color: var(--failed);
  }

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
    border-bottom: var(--border-hairline) solid var(--line);
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

  .note {
    margin: calc(var(--unit) * 4) 0 0;
    font-size: var(--type-control-size);
  }

  .narrowed {
    margin: 0 0 calc(var(--unit) * 3);
    font-size: var(--type-control-size);
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
