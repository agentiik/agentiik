<script lang="ts">
  import { untrack } from "svelte";
  import { refusal, type API, type Namespace } from "../api/client";
  import type { components } from "../api/schema";
  import Chart, { type Series } from "../components/Chart.svelte";
  import Icon from "../components/Icon.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Pane from "../components/Pane.svelte";
  import RangeBar from "../components/RangeBar.svelte";
  import type { Place } from "../lib/place.svelte";
  import { query, Ranged } from "../lib/range.svelte";
  import { bytes, ms, type Range } from "../lib/stats";

  // A namespace's statistics: its runs, as GET /api/v1/{ns}/stats/runs counts the ones the caller may
  // read, and its load against its quotas, as GET /api/v1/{ns}/stats/quotas answers whoever reads the
  // namespace. Every chart of the page is drawn over one range, which the address holds.
  let { api, place, namespace, record }: { api: API; place: Place; namespace: string; record: Namespace | undefined } = $props();

  type RunsSeries = components["schemas"]["statsRuns"];
  type QuotasSeries = components["schemas"]["statsQuotas"];

  const tab = $derived(place.query.get("tab") === "quotas" ? "quotas" : "runs");
  const ranged = new Ranged(() => place);
  const range = $derived(ranged.range);

  let runs = $state<RunsSeries | null>(null);
  let quotas = $state<QuotasSeries | null>(null);
  let refused = $state("");

  async function read(which: "runs" | "quotas", r: Range) {
    refused = "";
    if (which === "runs") {
      const { data, error, response } = await api.GET("/api/v1/{ns}/stats/runs", { params: { path: { ns: namespace }, query: query(r) } });
      if (data && typeof data !== "string") runs = data;
      else refused = refusal(response, error).message;
    } else {
      const { data, error, response } = await api.GET("/api/v1/{ns}/stats/quotas", { params: { path: { ns: namespace }, query: query(r) } });
      if (data && typeof data !== "string") quotas = data;
      else refused = refusal(response, error).message;
    }
  }

  $effect(() => {
    const which = tab;
    const r = range;
    untrack(() => read(which, r));
  });

  const zoom = (from: Date, to: Date) => ranged.zoom(from, to);
  const back = () => ranged.back();

  function show(which: "runs" | "quotas") {
    const q = new URLSearchParams(place.query);
    if (which === "quotas") q.set("tab", "quotas");
    else q.delete("tab");
    place.narrow(q);
  }

  // A point opens the runs it counts: the runs view narrowed to the bucket's bounds, as GET
  // /api/v1/runs takes them.
  function open(bucket: { since: string; until: string }, state?: string) {
    const q = new URLSearchParams({ since: bucket.since, until: bucket.until });
    if (state) q.set("state", state);
    place.go({ kind: "namespace", namespace, view: "runs" }, false, q);
  }

  // The series behind the page, as the route answered them, never the picture.
  async function exported(kind: "csv" | "json") {
    const path = tab === "runs" ? "/api/v1/{ns}/stats/runs" : "/api/v1/{ns}/stats/quotas";
    const { data, error, response } = await api.GET(path, {
      params: { path: { ns: namespace }, query: query(range) },
      headers: { Accept: kind === "csv" ? "text/csv" : "application/json" },
      parseAs: "blob",
    });
    if (!data) {
      refused = refusal(response, error).message;
      return;
    }
    const link = document.createElement("a");
    link.href = URL.createObjectURL(data as Blob);
    link.download = `${namespace}-${tab}-${range.from.toISOString().slice(0, 10)}.${kind}`;
    link.click();
    URL.revokeObjectURL(link.href);
  }

  const width = (b: string | undefined) => ({ "1m": 60, "15m": 900, "1h": 3600, "1d": 86400 })[b ?? "1h"] ?? 3600;
  const count = (n: number) => String(Math.round(n));

  // The previous span, drawn under the range's own buckets one for one, which the API makes possible
  // by answering it in as many buckets of the same length.
  function beside<T>(previous: T[] | undefined, pick: (b: T) => number | null | undefined): (number | null)[] | undefined {
    return previous?.map((b) => pick(b) ?? null);
  }

  const runSeries = $derived.by(() => {
    if (!runs) return null;
    const b = runs.buckets;
    const state = (s: string) => b.map((x) => (x.runs as Record<string, number>)[s] ?? 0);
    const going = b.map((x) => {
      const r = x.runs as Record<string, number>;
      return (r.running ?? 0) + (r.queued ?? 0) + (r.waiting ?? 0);
    });
    const byState: Series[] = [
      { label: "succeeded", tone: "succeeded", kind: "bars", values: state("succeeded") },
      { label: "failed", tone: "failed", kind: "bars", values: state("failed") },
      { label: "timed_out", tone: "waiting", kind: "bars", values: state("timed_out") },
      { label: "cancelled", tone: "quiet", kind: "bars", values: state("cancelled") },
      { label: "still going", tone: "running", kind: "bars", values: going },
    ];
    const pct = (p: "p50" | "p95" | "p99", of: "duration_ms" | "queue_wait_ms") => b.map((x) => x[of]?.[p] ?? null);
    const previous = range.compare ? runs.previous?.buckets : undefined;
    const duration: Series[] = [
      { label: "p50", tone: "accent", kind: "line", values: pct("p50", "duration_ms") },
      { label: "p95", tone: "accent-2", kind: "line", values: pct("p95", "duration_ms") },
      { label: "p99", tone: "accent-3", kind: "line", values: pct("p99", "duration_ms") },
    ];
    const wait: Series[] = [
      { label: "p50", tone: "accent", kind: "line", values: pct("p50", "queue_wait_ms") },
      { label: "p95", tone: "accent-2", kind: "line", values: pct("p95", "queue_wait_ms") },
    ];
    if (previous) {
      duration.push({ label: "p95, the span before", tone: "accent-2", kind: "line", dashed: true, values: beside(previous, (x) => x.duration_ms?.p95)! });
      wait.push({ label: "p95, the span before", tone: "accent-2", kind: "line", dashed: true, values: beside(previous, (x) => x.queue_wait_ms?.p95)! });
    }
    const retries = new Map<string, number>();
    for (const x of b) {
      for (const r of x.retries) {
        const code = r.exit_code === null || r.exit_code === undefined ? "lost" : String(r.exit_code);
        retries.set(code, (retries.get(code) ?? 0) + r.attempts);
      }
    }
    const total = b.reduce((n, x) => n + Object.values(x.runs as Record<string, number>).reduce((a, v) => a + v, 0), 0);
    return { since: b.map((x) => x.since), byState, duration, wait, retries: [...retries.entries()].sort((a, c) => c[1] - a[1]), total };
  });

  const quotaSeries = $derived.by(() => {
    if (!quotas) return null;
    const b = quotas.buckets;
    const previous = range.compare ? quotas.previous?.buckets : undefined;
    const created: Series[] = [
      { label: "created", tone: "accent", kind: "bars", values: b.map((x) => x.runs_created) },
      { label: "refused for max_runs_per_hour", tone: "failed", kind: "bars", values: b.map((x) => x.runs_refused) },
    ];
    const flight: Series[] = [{ label: "tasks in flight, at most", tone: "accent", kind: "step", values: b.map((x) => x.tasks_in_flight_max) }];
    const held: Series[] = [{ label: "artifact bytes", tone: "accent", kind: "area", values: b.map((x) => x.artifact_bytes) }];
    const added: Series[] = [{ label: "added", tone: "accent-2", kind: "bars", values: b.map((x) => x.artifact_bytes_added) }];
    if (previous) {
      flight.push({ label: "the span before", tone: "accent-2", kind: "step", dashed: true, values: beside(previous, (x) => x.tasks_in_flight_max)! });
      held.push({ label: "the span before", tone: "accent-2", kind: "line", dashed: true, values: beside(previous, (x) => x.artifact_bytes)! });
    }
    return { since: b.map((x) => x.since), created, flight, held, added };
  });

  const series = $derived(tab === "runs" ? runs : quotas);
  const buckets = $derived(series?.buckets ?? []);
  const limits = $derived(quotas?.quotas ?? record?.quotas);
</script>

<PageHeader title="Statistics" icon="control-statistics" {place} tabs={[
  { label: "Runs", icon: "control-runs", current: tab === "runs", onclick: () => show("runs") },
  { label: "Quotas", icon: "control-settings", current: tab === "quotas", onclick: () => show("quotas") },
]}>
  {#snippet actions()}
    <button class="control" title="Export the series as CSV" onclick={() => exported("csv")}><Icon name="control-download" size={14} />CSV</button>
    <button class="control" title="Export the series as JSON" onclick={() => exported("json")}><Icon name="control-download" size={14} />JSON</button>
  {/snippet}
</PageHeader>

<RangeBar {ranged} bucket={series?.bucket} />

{#if refused}
  <p class="refused" role="alert">The series could not be read: {refused}</p>
{/if}

{#if tab === "runs" && runSeries}
  {#key runSeries}
    <div class="grid">
      <Pane title="Runs by state" aside="{runSeries.total} runs created over the range">
        <Chart
          title="Runs created in each bucket, by where they stand"
          since={runSeries.since}
          width={width(runs?.bucket)}
          series={runSeries.byState}
          stacked
          format={count}
          onzoom={zoom}
          onback={back}
          onpick={(i) => buckets[i] && open(buckets[i])}
        />
      </Pane>
      <Pane title="Retries by exit code" aside="attempts over the range">
        {#if runSeries.retries.length === 0}
          <p class="muted">No attempt was retried over the range.</p>
        {:else}
          {@const most = runSeries.retries[0]![1]}
          <ul class="codes">
            {#each runSeries.retries as [code, n] (code)}
              <li>
                <span class="term code">{code}</span>
                <span class="bar" style:width="{(n / most) * 100}%"></span>
                <span class="term">{n}</span>
              </li>
            {/each}
          </ul>
        {/if}
      </Pane>
      <Pane title="Duration" aside="p50, p95 and p99 of the runs that ended">
        <Chart title="How long the runs took" since={runSeries.since} width={width(runs?.bucket)} series={runSeries.duration} format={ms} onzoom={zoom} onback={back} onpick={(i) => buckets[i] && open(buckets[i])} />
      </Pane>
      <Pane title="Queue wait" aside="from when a task could be handed out to its dispatch">
        <Chart title="How long the tasks waited for a runner" since={runSeries.since} width={width(runs?.bucket)} series={runSeries.wait} format={ms} onzoom={zoom} onback={back} onpick={(i) => buckets[i] && open(buckets[i])} />
      </Pane>
    </div>
  {/key}
{:else if tab === "quotas" && quotaSeries}
  {#key quotaSeries}
    <div class="grid quotas">
      <div class="charts">
        <Pane title="Runs created and refused" aside="against max_runs_per_hour">
          <Chart
            title="Runs created in each bucket, and those refused for the quota"
            since={quotaSeries.since}
            width={width(quotas?.bucket)}
            series={quotaSeries.created}
            stacked
            limit={quotas?.bucket === "1h" && limits?.max_runs_per_hour ? { label: "max_runs_per_hour", value: limits.max_runs_per_hour } : undefined}
            format={count}
            onzoom={zoom}
            onback={back}
            onpick={(i) => buckets[i] && open(buckets[i])}
          />
        </Pane>
        <Pane title="Tasks in flight" aside="the most at once in each bucket, against max_concurrent_tasks">
          <Chart
            title="Tasks handed out and not yet ended, at most"
            since={quotaSeries.since}
            width={width(quotas?.bucket)}
            series={quotaSeries.flight}
            limit={limits?.max_concurrent_tasks ? { label: "max_concurrent_tasks", value: limits.max_concurrent_tasks } : undefined}
            format={count}
            onzoom={zoom}
            onback={back}
          />
        </Pane>
        <Pane title="Artifact bytes" aside="held at each bucket's end, against max_artifact_bytes">
          <Chart
            title="Artifact bytes the quota counts"
            since={quotaSeries.since}
            width={width(quotas?.bucket)}
            series={quotaSeries.held}
            limit={limits?.max_artifact_bytes ? { label: "max_artifact_bytes", value: limits.max_artifact_bytes } : undefined}
            format={bytes}
            onzoom={zoom}
            onback={back}
          />
          <Chart title="Artifact bytes written in each bucket" since={quotaSeries.since} width={width(quotas?.bucket)} series={quotaSeries.added} format={bytes} height={140} onzoom={zoom} onback={back} />
        </Pane>
      </div>
      <Pane title="Quotas" aside="now">
        <dl class="limits">
          {#each Object.entries(limits ?? {}) as [name, value] (name)}
            <dt class="term">{name}</dt>
            <dd class="term">{Array.isArray(value) ? value.join(", ") : name.includes("bytes") ? bytes(Number(value)) : String(value)}</dd>
          {/each}
        </dl>
        <p class="faint">The quotas are an administrator's to change, with PUT /api/v1/namespaces/{namespace}/quotas.</p>
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
    grid-template-columns: 1.6fr 1fr;
    gap: calc(var(--unit) * 12) calc(var(--unit) * 7);
  }

  .grid.quotas {
    grid-template-columns: 1fr 300px;
    align-items: start;
  }

  .charts {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 12);
  }

  .codes {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .codes li {
    display: grid;
    grid-template-columns: 48px 1fr 40px;
    align-items: center;
    gap: calc(var(--unit) * 5);
    padding: calc(var(--unit) * 3) 0;
    font-size: var(--type-identifier-size-max);
  }

  .codes .bar {
    height: 10px;
    border-radius: 2px;
    background: var(--failed);
  }

  .limits {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: calc(var(--unit) * 4) calc(var(--unit) * 6);
    margin: 0 0 calc(var(--unit) * 8);
    font-size: var(--type-identifier-size-min);
  }

  .limits dd {
    margin: 0;
    text-align: right;
  }

  /* Under 1100px, where the sidebar folds, the two columns go one above the other. */
  @media (max-width: 1099px) {
    .grid,
    .grid.quotas {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
