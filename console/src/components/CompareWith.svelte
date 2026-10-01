<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { refusal, type API } from "../api/client";
  import { clock } from "../lib/format";
  import { follow, type Place } from "../lib/place.svelte";
  import { candidates } from "../lib/run-diff";
  import type { RunDetail } from "../lib/run.svelte";

  // The runs one run may be read against: the others of its workflow and its commit that have ended,
  // listed when asked for rather than with the run, a succeeded one offered first where this one did
  // not succeed, since a failed run is read against a good one.
  let { api, place, run }: { api: API; place: Place; run: RunDetail } = $props();

  type Listed = { run: string; commit: string; state: string; created_at: string; triggered_by?: string };
  let open = $state(false);
  let listed = $state<Listed[] | null>(null);
  let chosen = $state("");
  let refused = $state<Explained | null>(null);

  async function ask() {
    open = true;
    if (listed) return;
    const { data, error, response } = await api.GET("/api/v1/runs", { params: { query: { namespace: run.namespace, workflow: run.workflow, limit: 200 } } });
    if (!data) {
      refused = explain("load the runs to compare with", refusal(response, error));
      return;
    }
    listed = candidates(run, data.runs);
    chosen = listed[0]?.run ?? "";
  }

  const target = $derived({ kind: "namespace" as const, namespace: run.namespace, view: "runs" as const, run: run.run, against: chosen });
</script>

{#if !open}
  <button class="control" onclick={ask}>Compare with another run</button>
{:else if refused}
  <Problem explained={refused} />
{:else if listed && listed.length === 0}
  <span class="muted">No other run of {run.workflow}@{run.commit.slice(0, 7)} has ended to compare it with.</span>
{:else if listed}
  <label class="select">
    <span class="muted">Compare with</span>
    <select bind:value={chosen}>
      {#each listed as r (r.run)}<option value={r.run}>{r.state} · {r.run} · {clock(r.created_at, Date.now())}</option>{/each}
    </select>
  </label>
  <a class="control" href={place.href(target)} onclick={follow(place, target)}>Compare</a>
{:else}
  <span class="muted" role="status">Listing the runs of {run.workflow}@{run.commit.slice(0, 7)}</span>
{/if}

<style>
  .select {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .select select {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-family: var(--type-identifier-font);
    font-size: var(--type-control-size);
  }

</style>
