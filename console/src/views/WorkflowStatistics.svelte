<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { untrack } from "svelte";
  import { refusal, type API } from "../api/client";
  import Chart, { type Series } from "../components/Chart.svelte";
  import Icon from "../components/Icon.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Pane from "../components/Pane.svelte";
  import RangeBar from "../components/RangeBar.svelte";
  import { band } from "../lib/exit";
  import { took } from "../lib/format";
  import { workflowTabs } from "../lib/page";
  import type { Place } from "../lib/place.svelte";
  import { runsOf } from "../lib/route";
  import { query, Ranged } from "../lib/range.svelte";
  import { ms, type Range } from "../lib/stats";
  import { chosen, count, exitCodes, figures, heat, notSucceeded, refused, type PortsSeries, type RunsSeries, type StepsSeries } from "../lib/workflow-stats";

  // A workflow's statistics: its runs, as GET /api/v1/{ns}/stats/runs counts them narrowed to it, its
  // steps, as /stats/steps answers them bucket by bucket and by the hour of the week, and what they
  // published, as /stats/ports does. Every chart of the page is drawn over one range, which the
  // address holds, and the step the exit codes, the heatmap and the last figure are of is named
  // there too.
  // graph says whether the caller reads the workflow itself, which its graph takes and its series do
  // not: the tab is left out for one who reads only its runs.
  let { api, place, namespace, workflow, graph = false, settles = false }: { api: API; place: Place; namespace: string; workflow: string; graph?: boolean; settles?: boolean } = $props();
  const tabs = $derived(workflowTabs(namespace, workflow, "statistics", { mcp: false, settles }).filter((t) => graph || t.label !== "Graph"));

  const ranged = new Ranged(() => place);
  const range = $derived(ranged.range);

  let runs = $state<RunsSeries | null>(null);
  let steps = $state<StepsSeries | null>(null);
  let hours = $state<StepsSeries | null>(null);
  let ports = $state<PortsSeries | null>(null);
  let refusedWith = $state<Explained | null>(null);

  async function read(r: Range) {
    refusedWith = null;
    const path = { ns: namespace };
    const q = { ...query(r), workflow };
    const [a, b, c, d] = await Promise.all([
      api.GET("/api/v1/{ns}/stats/runs", { params: { path, query: q } }),
      api.GET("/api/v1/{ns}/stats/steps", { params: { path, query: q } }),
      api.GET("/api/v1/{ns}/stats/steps", { params: { path, query: { from: q.from, range: q.range, to: q.to, workflow, by: "hour" } } }),
      api.GET("/api/v1/{ns}/stats/ports", { params: { path, query: q } }),
    ]);
    for (const answer of [a, b, c, d]) {
      if (!answer.data || typeof answer.data === "string") {
        refusedWith = explain("load the statistics", refusal(answer.response, answer.error));
        return;
      }
    }
    runs = a.data as RunsSeries;
    steps = b.data as StepsSeries;
    hours = c.data as StepsSeries;
    ports = d.data as PortsSeries;
  }

  $effect(() => {
    const r = range;
    untrack(() => read(r));
  });

  const step = $derived(steps ? chosen(steps.steps, place.query.get("step")) : undefined);

  function choose(name: string) {
    const q = new URLSearchParams(place.query);
    q.set("step", name);
    place.narrow(q);
  }

  // A point opens the runs it counts: the workflow's runs narrowed to the bucket's bounds, as GET
  // /api/v1/runs takes them.
  function open(bucket: { since: string; until: string } | undefined) {
    if (!bucket) return;
    place.go(runsOf(namespace, workflow), false, new URLSearchParams({ since: bucket.since, until: bucket.until }));
  }

  // The series behind the page, one route's answer at a time, as it answered them.
  const exports = {
    runs: { label: "runs", path: "/api/v1/{ns}/stats/runs", by: undefined },
    steps: { label: "steps", path: "/api/v1/{ns}/stats/steps", by: undefined },
    hours: { label: "steps by hour", path: "/api/v1/{ns}/stats/steps", by: "hour" },
    ports: { label: "ports", path: "/api/v1/{ns}/stats/ports", by: undefined },
  } as const;
  let exporting = $state<keyof typeof exports>("runs");

  async function exported(kind: "csv" | "json") {
    const which = exports[exporting];
    const q = which.by ? { from: query(range).from, range: query(range).range, to: query(range).to, workflow, by: which.by } : { ...query(range), workflow };
    const { data, error, response } = await api.GET(which.path, {
      params: { path: { ns: namespace }, query: q },
      headers: { Accept: kind === "csv" ? "text/csv" : "application/json" },
      parseAs: "blob",
    });
    if (!data) {
      refusedWith = explain("load the statistics", refusal(response, error));
      return;
    }
    const link = document.createElement("a");
    link.href = URL.createObjectURL(data as Blob);
    link.download = `${namespace}-${workflow}-${exporting}-${(runs?.from ?? range.from.toISOString()).slice(0, 10)}.${kind}`;
    link.click();
    URL.revokeObjectURL(link.href);
  }

  const width = (b: string | undefined) => ({ "1m": 60, "15m": 900, "1h": 3600, "1d": 86400 })[b ?? "1h"] ?? 3600;
  const pct = (n: number) => `${n.toFixed(n < 10 && n !== 0 ? 1 : 0)}%`;

  const drawn = $derived.by(() => {
    if (!runs) return null;
    const b = runs.buckets;
    const previous = range.compare ? runs.previous?.buckets : undefined;
    const shares = notSucceeded(b);
    const failing: Series[] = [
      { label: "failed", tone: "failed", kind: "bars", values: shares.failed },
      { label: "timed_out", tone: "waiting", kind: "bars", values: shares.timed_out },
      { label: "cancelled", tone: "quiet", kind: "bars", values: shares.cancelled },
    ];
    if (previous) failing.push({ label: "all three, the span before", tone: "accent-2", kind: "line", dashed: true, values: notSucceeded(previous).all });
    const p = (which: "p50" | "p95" | "p99", of: "duration_ms" | "queue_wait_ms", from = b) => from.map((x) => x[of]?.[which] ?? null);
    const duration: Series[] = [
      { label: "p50", tone: "accent", kind: "line", values: p("p50", "duration_ms") },
      { label: "p95", tone: "accent-2", kind: "line", values: p("p95", "duration_ms") },
      { label: "p99", tone: "accent-3", kind: "line", values: p("p99", "duration_ms") },
    ];
    const wait: Series[] = [
      { label: "p50", tone: "accent", kind: "line", values: p("p50", "queue_wait_ms") },
      { label: "p95", tone: "accent-2", kind: "line", values: p("p95", "queue_wait_ms") },
    ];
    if (previous) {
      duration.push({ label: "p95, the span before", tone: "accent-2", kind: "line", dashed: true, values: p("p95", "duration_ms", previous) });
      wait.push({ label: "p95, the span before", tone: "accent-2", kind: "line", dashed: true, values: p("p95", "queue_wait_ms", previous) });
    }
    return { since: b.map((x) => x.since), failing, duration, wait };
  });

  const shown = $derived(runs ? figures(runs, step, range.compare) : []);
  const codes = $derived(step ? exitCodes(step.buckets ?? []) : []);
  const attempts = $derived(step ? (step.buckets ?? []).reduce((a, b) => a + b.attempts, 0) : 0);

  const days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const week = $derived.by(() => {
    const cells = hours?.steps.find((s) => s.step === step?.step)?.hours ?? [];
    const at = (d: number, h: number) => cells.find((c) => c.weekday === d && c.hour === h)?.duration_ms?.p50;
    const scale = heat(cells.map((c) => c.duration_ms?.p50));
    return { at, scale };
  });
  let pointed = $state<{ day: number; hour: number } | null>(null);

  const items = $derived(ports ? refused(ports) : null);
  const itemLines = $derived<Series[]>(
    items?.lines.map((l) => ({ label: l.port === "rejected" ? `rejected by ${l.step}` : `error from ${l.step}`, tone: l.port === "rejected" ? "waiting" : "failed", kind: "line", values: l.values })) ?? [],
  );
  const portsSince = $derived(ports?.steps[0]?.buckets.map((b) => b.since) ?? drawn?.since ?? []);
</script>

<PageHeader title={workflow} icon="control-workflows" {place} {tabs}>
  {#snippet actions()}
    {#if steps && steps.steps.length > 0}
      <label class="select step">
        <span class="muted">Step</span>
        <select value={step?.step} onchange={(e) => choose(e.currentTarget.value)}>
          {#each steps.steps as s (s.step)}<option value={s.step}>{s.step}</option>{/each}
        </select>
        <Icon name="control-expand" size={14} />
      </label>
    {/if}
  {/snippet}
</PageHeader>

<RangeBar {ranged} bucket={runs?.bucket} from={runs?.from}>
  <label class="select">
    <span class="muted">Export</span>
    <select bind:value={exporting}>
      {#each Object.entries(exports) as [key, e] (key)}<option value={key}>{e.label}</option>{/each}
    </select>
    <Icon name="control-expand" size={14} />
  </label>
  <button class="control" onclick={() => exported("csv")}><Icon name="control-download" size={14} />CSV</button>
  <button class="control" onclick={() => exported("json")}><Icon name="control-download" size={14} />JSON</button>
</RangeBar>

{#if refusedWith}
  <Problem explained={refusedWith} onretry={() => read(range)} />
{/if}

{#if runs && drawn}
  {#key drawn}
    <dl class="figures">
      {#each shown as f (f.label)}
        <div class="figure">
          <dt>{f.label}</dt>
          <dd>
            <span class="value term">{f.value}</span>
            {#if f.change}<span class="change term" title="against the span before">{f.change}</span>{/if}
          </dd>
          <dd class="aside muted">{f.aside}</dd>
        </div>
      {/each}
    </dl>

    <div class="grid">
      <Pane title="Runs that did not succeed">
        <Chart title="Runs that failed, timed out or were cancelled, of those that ended" since={drawn.since} width={width(runs.bucket)} series={drawn.failing} stacked format={pct} onzoom={(f, t) => ranged.zoom(f, t)} onback={() => ranged.back()} onpick={(i) => open(runs?.buckets[i])} />
      </Pane>
      <Pane title={step ? `Exit codes of ${step.step}` : "Exit codes"} aside={step ? `${count(codes.reduce((a, c) => a + c.attempts, 0))} of ${count(attempts)} attempts failed` : undefined}>
        {#if codes.length === 0}
          <p class="muted">No failures</p>
        {:else}
          {@const most = codes[0]!.attempts}
          <ul class="codes">
            {#each codes as c (c.code)}
              {@const b = c.code === null ? null : band(c.code)}
              <li title={b ? b.handling : "lost with its runner, or stopped before a container reported a code"}>
                <span class="term code">{c.code ?? "lost"}</span>
                <span class="muted meaning">{b ? b.name : "no exit code"}</span>
                <span class="bar {b ? b.tone : 'quiet'}" style:width="{(c.attempts / most) * 100}%"></span>
                <span class="term">{count(c.attempts)}</span>
              </li>
            {/each}
          </ul>
        {/if}
      </Pane>
      <Pane title="Duration">
        <Chart title="How long the runs took" since={drawn.since} width={width(runs.bucket)} series={drawn.duration} format={ms} onzoom={(f, t) => ranged.zoom(f, t)} onback={() => ranged.back()} onpick={(i) => open(runs?.buckets[i])} />
      </Pane>
      <Pane title="Queue wait">
        <Chart title="How long the tasks waited for a runner" since={drawn.since} width={width(runs.bucket)} series={drawn.wait} format={ms} onzoom={(f, t) => ranged.zoom(f, t)} onback={() => ranged.back()} onpick={(i) => open(runs?.buckets[i])} />
      </Pane>
      <Pane title={step ? `${step.step} by hour of day` : "By hour of day"} aside="UTC">
        {#if week.scale.levels.length === 0}
          <p class="muted">No data</p>
        {:else}
          <table class="heat tiled" aria-label="The p50 duration of {step?.step} by weekday and hour, in UTC">
            <thead>
              <tr>
                <th scope="col" class="day"><span class="unseen">Weekday</span></th>
                {#each Array.from({ length: 24 }, (_, h) => h) as h (h)}<th scope="col" class="hour term">{h % 3 === 0 ? String(h).padStart(2, "0") : ""}<span class="unseen">{h % 3 === 0 ? "" : String(h).padStart(2, "0")}</span></th>{/each}
              </tr>
            </thead>
            <tbody>
              {#each days as name, d (name)}
                <tr>
                  <th scope="row" class="day">{name}</th>
                  {#each Array.from({ length: 24 }, (_, h) => h) as h (h)}
                    {@const p50 = week.at(d + 1, h)}
                    <td
                      class="cell level-{week.scale.level(p50)}"
                      class:pointed={pointed?.day === d && pointed?.hour === h}
                      onmouseenter={() => (pointed = { day: d, hour: h })}
                      onmouseleave={() => (pointed = null)}
                    ><span class="unseen">{p50 === undefined ? "no run" : took(p50)}</span></td>
                  {/each}
                </tr>
              {/each}
            </tbody>
          </table>
          <div class="legend">
            {#each week.scale.levels as low, i (i)}
              <span><span class="swatch level-{week.scale.levels.length === 1 ? 5 : i + 1}"></span><span class="term">{i === week.scale.levels.length - 1 && i > 0 ? `≥ ${took(low)}` : took(low)}</span></span>
            {/each}
            <span><span class="swatch level-0"></span>no run</span>
          </div>
          <p class="readout" aria-live="polite">
            {#if pointed}
              {@const p50 = week.at(pointed.day + 1, pointed.hour)}
              <span class="term">{days[pointed.day]} {String(pointed.hour).padStart(2, "0")}:00 to {String(pointed.hour + 1).padStart(2, "0")}:00 UTC</span>
              <span>p50 <span class="term">{p50 === undefined ? "no run" : took(p50)}</span></span>
            {/if}
          </p>
        {/if}
      </Pane>
      <Pane title="Items refused" aside={items && items.lines.length > 0 ? `${count(items.rejected)} rejected, ${count(items.error)} to error, of ${count(items.of)}` : undefined}>
        {#if !items || items.lines.length === 0}
          <p class="muted">Nothing refused</p>
        {:else}
          <Chart title="The share of each step's items rejected or sent to error" since={portsSince} width={width(ports?.bucket)} series={itemLines} format={pct} onzoom={(f, t) => ranged.zoom(f, t)} onback={() => ranged.back()} onpick={(i) => open(runs?.buckets[i])} />
        {/if}
      </Pane>
    </div>
  {/key}
{/if}

<style>

  .select {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    color: var(--muted);
    font-size: var(--type-control-size);
    white-space: nowrap;
  }

  .select select {
    appearance: none;
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 14) 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .select :global(.icon) {
    position: absolute;
    right: calc(var(--unit) * 4);
    pointer-events: none;
  }

  .step {
    margin-left: auto;
  }

  .refused {
    color: var(--failed);
  }

  .figures {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
    gap: calc(var(--unit) * 7);
    margin: 0 0 calc(var(--unit) * 12);
  }

  .figure {
    padding: calc(var(--unit) * 6) calc(var(--unit) * 7);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-pane);
    background: var(--raised);
  }

  .figure dt {
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .figure dd {
    margin: calc(var(--unit) * 2) 0 0;
  }

  .value {
    font-size: 22px;
    font-weight: 600;
  }

  .change {
    margin-left: calc(var(--unit) * 4);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .aside {
    font-size: var(--type-control-size);
  }

  .grid {
    display: grid;
    grid-template-columns: 1.6fr 1fr;
    gap: calc(var(--unit) * 12) calc(var(--unit) * 7);
  }

  .codes {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .codes li {
    display: grid;
    grid-template-columns: 44px 150px 1fr 40px;
    align-items: center;
    gap: calc(var(--unit) * 5);
    padding: calc(var(--unit) * 3) 0;
    font-size: var(--type-identifier-size-max);
  }

  .meaning {
    font-size: var(--type-control-size);
  }

  .codes .bar {
    height: 10px;
    border-radius: 2px;
    background: var(--failed);
  }

  .codes .bar.waiting {
    background: var(--waiting);
  }

  .codes .bar.quiet {
    background: var(--faint);
  }

  .faint {
    margin: calc(var(--unit) * 5) 0 0;
    font-size: var(--type-control-size);
  }

  /* The spacing between cells is taken back at the edges, so that the weekdays start where the pane's
     title does. */
  .heat {
    width: calc(100% + 4px);
    margin: 0 -2px;
    border-collapse: separate;
    border-spacing: 2px;
    table-layout: fixed;
  }

  .heat th {
    padding: 0;
    color: var(--faint);
    font-size: 10px;
    font-weight: 400;
    text-align: left;
  }

  /* The table lays its columns out from its first row, so the weekdays' width is set on the head's
     first cell too, wide enough for the widest of them. */
  .heat .day {
    width: 40px;
    font-size: var(--type-control-size);
  }

  .cell {
    height: 16px;
    border-radius: 2px;
    background: var(--sunken);
  }

  .cell.pointed {
    outline: var(--border-focus) solid var(--text);
  }

  .level-0 {
    background: var(--sunken);
  }

  .level-1 {
    background: color-mix(in srgb, var(--accent) 16%, var(--sunken));
  }

  .level-2 {
    background: color-mix(in srgb, var(--accent) 34%, var(--sunken));
  }

  .level-3 {
    background: color-mix(in srgb, var(--accent) 52%, var(--sunken));
  }

  .level-4 {
    background: color-mix(in srgb, var(--accent) 74%, var(--sunken));
  }

  .level-5 {
    background: var(--accent);
  }

  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    margin-top: calc(var(--unit) * 5);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .swatch {
    display: inline-block;
    width: 10px;
    height: 10px;
    margin-right: calc(var(--unit) * 2);
    border-radius: 2px;
    vertical-align: -1px;
  }

  .readout {
    display: flex;
    gap: calc(var(--unit) * 7);
    min-height: 1lh;
    margin: calc(var(--unit) * 3) 0 0;
    font-size: var(--type-control-size);
  }

  /* Under 1100px, where the sidebar folds, the two columns go one above the other. */
  @media (max-width: 1099px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  @media (max-width: 759px) {
    .codes li {
      grid-template-columns: 44px minmax(0, 1fr) 40px;
    }

    .codes .meaning {
      display: none;
    }
  }
</style>
