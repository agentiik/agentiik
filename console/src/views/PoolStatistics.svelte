<script lang="ts">
  import PageHeader from "../components/PageHeader.svelte";
  import { untrack } from "svelte";
  import { refusal, type API } from "../api/client";
  import Chart, { type Series } from "../components/Chart.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import RangeBar from "../components/RangeBar.svelte";
  import { took } from "../lib/format";
  import { follow, type Place } from "../lib/place.svelte";
  import { declaredLost, marks, ticks, usage, type Mark, type PoolsSeries } from "../lib/pool-stats";
  import { query, Ranged } from "../lib/range.svelte";
  import type { Range } from "../lib/stats";

  // The statistics of the pools and the runners, an administrator's alone, from GET
  // /api/v1/stats/pools: each pool's slots in use against what its runners offered, which falls as
  // a runner drains, and each silence between a runner's heartbeats of two intervals or more, those
  // after which its tasks were declared lost set apart. The route takes no comparison: what a pool
  // is set against is its capacity.
  let { api, place }: { api: API; place: Place } = $props();

  const ranged = new Ranged(() => place);
  const inventory = { kind: "runners" as const };
  const range = $derived(ranged.range);

  let pools = $state<PoolsSeries | null>(null);
  let refused = $state("");

  async function read(r: Range) {
    refused = "";
    const { from, to } = query(r);
    const { data, error, response } = await api.GET("/api/v1/stats/pools", { params: { query: { from, to } } });
    if (data && typeof data !== "string") pools = data;
    else refused = refusal(response, error).message;
  }

  $effect(() => {
    const r = range;
    untrack(() => read(r));
  });

  async function exported(kind: "csv" | "json") {
    const { from, to } = query(range);
    const { data, error, response } = await api.GET("/api/v1/stats/pools", {
      params: { query: { from, to } },
      headers: { Accept: kind === "csv" ? "text/csv" : "application/json" },
      parseAs: "blob",
    });
    if (!data) {
      refused = refusal(response, error).message;
      return;
    }
    const link = document.createElement("a");
    link.href = URL.createObjectURL(data as Blob);
    link.download = `pools-${range.from.toISOString().slice(0, 10)}.${kind}`;
    link.click();
    URL.revokeObjectURL(link.href);
  }

  const width = (b: string | undefined) => ({ "1m": 60, "15m": 900, "1h": 3600, "1d": 86400 })[b ?? "1h"] ?? 3600;
  // A slot is whole: an axis tick between two is left unwritten rather than rounded onto one.
  const slots = (n: number) => (Number.isInteger(n) ? String(n) : "");
  const time = (at: string) => new Date(at).toISOString().slice(11, 16);
  const day = (at: string) => new Date(at).toISOString().slice(0, 16).replace("T", " ");

  function said(u: ReturnType<typeof usage>): string {
    if (u.share === undefined) return "no slot offered over the range";
    const parts = [`${Math.round(u.share)}% of slots in use`];
    if (u.peakAt) parts.push(`peak ${u.peak} of ${u.peakOf}, ${time(u.peakAt)}`);
    if (u.idleSince) parts.push(`no capacity since ${time(u.idleSince)}`);
    return parts.join(" · ");
  }

  const drawn = $derived(
    pools?.pools.map((p) => ({
      pool: p.pool,
      since: p.buckets.map((b) => b.since),
      aside: said(usage(p.buckets)),
      series: [
        { label: "slots in use", tone: "accent", kind: "area", values: p.buckets.map((b) => b.slots_in_use_max) },
        { label: "capacity", tone: "accent-2", kind: "step", dashed: true, values: p.buckets.map((b) => b.capacity) },
      ] satisfies Series[],
      runners: p.runners.map((r) => ({
        runner: r.runner,
        peak: Math.max(0, ...r.buckets.map((b) => b.slots_in_use_max)),
        capacity: r.buckets.at(-1)?.capacity ?? 0,
        silences: r.silences.length,
        lost: r.silences.reduce((n, s) => n + s.tasks_lost, 0),
      })),
    })) ?? [],
  );

  const gaps = $derived(
    pools ? pools.pools.flatMap((p) => p.runners.map((r) => ({ runner: r.runner, pool: p.pool, marks: marks(r.silences, pools!.from, pools!.to) }))) : [],
  );
  const axis = $derived(pools ? ticks(pools.from, pools.to) : []);
  let pointed = $state<{ runner: string; mark: Mark } | null>(null);

  function told(runner: string, m: Mark): string {
    const s = m.silence;
    const lost = s.tasks_lost === 1 ? "1 task declared lost" : `${s.tasks_lost} tasks declared lost`;
    return `${runner} silent ${took(s.length_ms)} from ${day(s.at)} UTC${m.lost ? `, ${lost}` : ""}`;
  }
</script>

<PageHeader title="Runners" icon="control-runners" {place} tabs={[
  { label: "Runners and pools", icon: "control-runners", to: { kind: "runners" }, current: false },
  { label: "Statistics", icon: "control-statistics", to: { kind: "runners", tab: "statistics" }, current: true },
]}>
  {#snippet actions()}
    <button class="control" title="Export the series as CSV" onclick={() => exported("csv")}><Icon name="control-download" size={14} />CSV</button>
    <button class="control" title="Export the series as JSON" onclick={() => exported("json")}><Icon name="control-download" size={14} />JSON</button>
  {/snippet}
</PageHeader>

<RangeBar {ranged} bucket={pools?.bucket} comparable={false} />

{#if refused}
  <p class="refused" role="alert">The series could not be read: {refused}</p>
{/if}

{#if pools}
  {#key pools}
    {#if drawn.length === 0}
      <p class="muted">No pool held a runner over the range.</p>
    {/if}
    <div class="grid">
      {#each drawn as p (p.pool)}
        <Pane title={p.pool} aside={p.aside}>
          <Chart title="Slots in use in {p.pool}, against its capacity" since={p.since} width={width(pools.bucket)} series={p.series} format={slots} height={170} onzoom={(f, t) => ranged.zoom(f, t)} onback={() => ranged.back()} />
          <details class="runners">
            <summary>Its runners</summary>
            <table>
              <thead>
                <tr><th>Runner</th><th class="number">Most in use</th><th class="number">Offered at the end</th><th class="number">Silences</th><th class="number">Tasks lost</th></tr>
              </thead>
              <tbody>
                {#each p.runners as r (r.runner)}
                  <tr><td class="term">{r.runner}</td><td class="number term">{r.peak}</td><td class="number term">{r.capacity}</td><td class="number term">{r.silences}</td><td class="number term">{r.lost}</td></tr>
                {:else}
                  <tr><td colspan="5" class="muted">No runner was in the pool over the range.</td></tr>
                {/each}
              </tbody>
            </table>
          </details>
        </Pane>
      {/each}
    </div>

    <div class="gaps">
      <Pane title="Heartbeat gaps" aside="silences of two intervals, 20 s, or more, by runner">
        <p class="legend">
          <span><span class="swatch"></span>20 to 30 s</span>
          <span><span class="swatch lost"></span>{declaredLost / 1000} s and over: its tasks declared lost</span>
        </p>
        <div class="timeline" role="list" aria-label="Silences between heartbeats, by runner">
          {#each gaps as g (g.runner)}
            <div class="row" role="listitem">
              <span class="term name" title="pool {g.pool}">{g.runner}</span>
              <span class="track">
                {#each g.marks as m (m.silence.at)}
                  <button
                    class="mark"
                    class:lost={m.lost}
                    style:left="{m.left}%"
                    style:width="{m.width}%"
                    aria-label={told(g.runner, m)}
                    onmouseenter={() => (pointed = { runner: g.runner, mark: m })}
                    onfocus={() => (pointed = { runner: g.runner, mark: m })}
                    onmouseleave={() => (pointed = null)}
                    onblur={() => (pointed = null)}
                  ></button>
                {/each}
                {#if g.marks.length === 0}<span class="unseen">no silence</span>{/if}
              </span>
            </div>
          {/each}
          <div class="row axis" aria-hidden="true">
            <span></span>
            <span class="track">
              {#each axis as t (t.at)}<span class="tick term" style:left="{t.at}%">{t.label}</span>{/each}
            </span>
          </div>
        </div>
        <p class="readout" aria-live="polite">{pointed ? told(pointed.runner, pointed.mark) : ""}</p>
      </Pane>
    </div>
  {/key}
{/if}

<style>

  .refused {
    color: var(--failed);
  }

  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(480px, 100%), 1fr));
    gap: calc(var(--unit) * 12) calc(var(--unit) * 7);
  }

  .gaps {
    margin-top: calc(var(--unit) * 12);
  }

  .runners {
    margin-top: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .runners summary {
    color: var(--muted);
    cursor: pointer;
  }

  table {
    width: 100%;
    margin-top: calc(var(--unit) * 3);
    border-collapse: collapse;
  }

  th,
  td {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
    text-align: left;
  }

  th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .number {
    text-align: right;
  }

  .legend {
    display: flex;
    gap: calc(var(--unit) * 8);
    margin: 0 0 calc(var(--unit) * 6);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .swatch {
    display: inline-block;
    width: 10px;
    height: 10px;
    margin-right: calc(var(--unit) * 2);
    border-radius: 2px;
    background: var(--waiting);
    vertical-align: -1px;
  }

  .swatch.lost {
    background: var(--failed);
  }

  .timeline {
    display: grid;
    gap: calc(var(--unit) * 2);
  }

  .row {
    display: grid;
    grid-template-columns: 180px 1fr;
    align-items: center;
    gap: calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .track {
    position: relative;
    height: 18px;
    border-radius: 2px;
    background: var(--sunken);
  }

  .axis .track {
    background: none;
  }

  .mark {
    position: absolute;
    top: 2px;
    bottom: 2px;
    min-width: 4px;
    padding: 0;
    border: none;
    border-radius: 2px;
    background: var(--waiting);
    cursor: pointer;
  }

  .mark.lost {
    background: var(--failed);
  }

  .mark:focus-visible {
    outline: var(--border-focus) solid var(--text);
    outline-offset: 1px;
  }

  .tick {
    position: absolute;
    top: 0;
    color: var(--faint);
    font-size: 11px;
    transform: translateX(-50%);
  }

  .readout {
    min-height: 1.4em;
    margin: calc(var(--unit) * 5) 0 0;
    font-size: var(--type-control-size);
  }

  @media (max-width: 759px) {
    .row {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
