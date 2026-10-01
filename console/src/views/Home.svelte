<script lang="ts">
  import { untrack } from "svelte";
  import type { API, Me, Namespace } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import { added, boundsOf, dayOf, failures, grid, lastWeek, months, said, shades, total, weeks, yearOf, type RunsSeries, type Square } from "../lib/activity";
  import { clock, took } from "../lib/format";
  import { useKeys } from "../lib/keys.svelte";
  import { holdsSomewhereIn, ordered } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import type { Run } from "../lib/runs.svelte";

  // The console's root: the caller's home, every namespace they read together, rather than one
  // namespace's runs, since what a person looks at first is what moved anywhere they work. A year of
  // activity at the top, a square a day; each namespace's last seven days; the last runs across them
  // all. Nothing here is a route of its own: the squares are each namespace's series by the day, added
  // together, and the runs are GET /api/v1/runs, which lists every namespace's at once.
  let { api, place, me, namespaces }: { api: API; place: Place; me: Me; namespaces: Namespace[] } = $props();

  const now = Date.now();
  const year = yearOf(now);

  // The namespaces read here: those the caller reads runs in, the personal one first, as the switcher
  // lists them.
  const read = $derived.by(() => {
    const { own, personal, shared } = ordered(namespaces, me.principal);
    return [...(own ? [own] : []), ...personal, ...shared].filter((n) => holdsSomewhereIn(me, "run:read", n.name));
  });

  let series = $state(new Map<string, RunsSeries>());
  let unread = $state<string[]>([]);
  let reading = $state(true);
  let recent = $state<Run[]>([]);

  async function readAll(names: string[]) {
    reading = true;
    const answers = await Promise.all(
      names.map(async (ns) => {
        const { data } = await api.GET("/api/v1/{ns}/stats/runs", {
          params: { path: { ns }, query: { from: year.from.toISOString(), to: year.to.toISOString(), bucket: "1d" } },
        });
        return [ns, data] as const;
      }),
    );
    const got = new Map<string, RunsSeries>();
    const missed: string[] = [];
    for (const [ns, data] of answers) {
      if (data) got.set(ns, data as RunsSeries);
      else missed.push(ns);
    }
    series = got;
    unread = missed;
    reading = false;
  }

  async function readRecent() {
    const { data } = await api.GET("/api/v1/runs", { params: { query: { limit: 10 } } });
    recent = data?.runs ?? [];
  }

  $effect(() => {
    const names = read.map((n) => n.name);
    untrack(() => {
      readAll(names);
      readRecent();
    });
  });

  // What shades the squares: every run, or the runs that failed or timed out.
  const by = $derived(place.query.get("by") === "failures" ? "failures" : "runs");
  const counts = $derived(added([...series.values()]));
  const columns = $derived(grid(now, counts));
  const valueOf = (s: Square) => (by === "runs" ? total(s.counts) : failures(s.counts));
  const scale = $derived(shades(columns.flat().filter((s) => !s.future).map(valueOf)));
  const labels = $derived(months(columns));
  const yearTotal = $derived(columns.flat().reduce((n, s) => n + valueOf(s), 0));

  // The day chosen, as ?day= in the address, and its runs across every namespace.
  const chosen = $derived.by(() => {
    const d = place.query.get("day");
    return d && /^\d{4}-\d{2}-\d{2}$/.test(d) ? columns.flat().find((s) => s.day === d && !s.future) : undefined;
  });
  let ofDay = $state<{ day: string; runs: Run[]; more: boolean } | null>(null);
  $effect(() => {
    const d = chosen?.day;
    if (!d) {
      ofDay = null;
      return;
    }
    untrack(async () => {
      const { since, until } = boundsOf(d);
      // One more than is shown, to know whether the day holds more.
      const { data } = await api.GET("/api/v1/runs", { params: { query: { since, until, limit: 101 } } });
      const runs = data?.runs ?? [];
      if (chosen?.day === d) ofDay = { day: d, runs: runs.slice(0, 100), more: runs.length > 100 };
    });
  });

  function choose(day: string | undefined) {
    const q = new URLSearchParams(place.query);
    if (day) q.set("day", day);
    else q.delete("day");
    place.narrow(q);
  }

  function shadeBy(value: "runs" | "failures") {
    const q = new URLSearchParams(place.query);
    if (value === "failures") q.set("by", "failures");
    else q.delete("by");
    place.narrow(q);
  }

  // The arrows move the day chosen along the grid as it is drawn: up and down a day, left and right a
  // week, never onto a day to come.
  const days = $derived(columns.flat().filter((s) => !s.future).map((s) => s.day));
  useKeys(() => [
    {
      keys: ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"],
      brief: ["ArrowLeft", "ArrowRight"],
      effect: "Day",
      does: (key) => {
        const at = chosen ? days.indexOf(chosen.day) : days.length;
        const step = key === "ArrowUp" ? -1 : key === "ArrowDown" ? 1 : key === "ArrowLeft" ? -7 : 7;
        const next = Math.max(0, Math.min(days.length - 1, at + step));
        if (!chosen && (key === "ArrowLeft" || key === "ArrowUp")) choose(days[days.length - 1]);
        else choose(days[next]);
      },
    },
    ...(chosen ? [{ keys: ["Escape"], effect: "Close the day", does: () => choose(undefined) }] : []),
  ]);

  const runRoute = (r: Run) => ({ kind: "namespace" as const, namespace: r.namespace, view: "runs" as const, run: r.run });
  const runsOf = (ns: string) => ({ kind: "namespace" as const, namespace: ns, view: "runs" as const });
  const lasted = (r: Run) => (r.started_at ? took(Math.max(0, (r.finished_at ? Date.parse(r.finished_at) : now) - Date.parse(r.started_at))) : "");
  const weekdays = ["Mon", "", "Wed", "", "Fri", "", ""];
  const name = $derived(me.user?.display_name ?? me.principal);
</script>

<h1 class="hello">{name}</h1>

<Pane title="Activity" aside={reading ? "reading" : `${yearTotal} ${by === "runs" ? (yearTotal === 1 ? "run" : "runs") : yearTotal === 1 ? "failure" : "failures"} in the last year, ${read.length} ${read.length === 1 ? "namespace" : "namespaces"}`}>
  {#if unread.length}<p class="refused">Not read: {unread.join(", ")}</p>{/if}
  <div class="year" class:failures={by === "failures"}>
    <div class="months" style:grid-template-columns="repeat({weeks}, var(--square))" aria-hidden="true">
      {#each labels as m (m.column)}<span style:grid-column="{m.column + 1} / span 3">{m.name}</span>{/each}
    </div>
    <div class="body">
      <div class="weekdays" aria-hidden="true">
        {#each weekdays as w, i (i)}<span>{w}</span>{/each}
      </div>
      <div class="squares" role="grid" aria-label="Runs a day over the last year">
        {#each columns as column, w (w)}
          <div class="week" role="row">
            {#each column as s (s.day)}
              {#if s.future}
                <span class="square future" role="gridcell" aria-hidden="true"></span>
              {:else}
                <button
                  class="square level-{scale.level(valueOf(s))}"
                  class:chosen={chosen?.day === s.day}
                  role="gridcell"
                  title={said(s, by)}
                  aria-label={said(s, by)}
                  aria-selected={chosen?.day === s.day}
                  onclick={() => choose(chosen?.day === s.day ? undefined : s.day)}
                ></button>
              {/if}
            {/each}
          </div>
        {/each}
      </div>
    </div>
    <div class="legend">
      <div class="by" role="group" aria-label="What shades the squares">
        <button class="tab" aria-pressed={by === "runs"} onclick={() => shadeBy("runs")}>Runs</button>
        <button class="tab" aria-pressed={by === "failures"} onclick={() => shadeBy("failures")}>Failures</button>
      </div>
      <span class="faint less">Less</span>
      {#each [0, 1, 2, 3, 4] as l (l)}<span class="square level-{l}" title={l === 0 ? "none" : `${scale.bounds[l - 1] ?? ""} or more`}></span>{/each}
      <span class="faint">More</span>
    </div>
  </div>

  {#if chosen}
    <section class="day" aria-label="Runs of {chosen.day}">
      <header>
        <strong>{said(chosen, "runs")}</strong>
        <button class="control" onclick={() => choose(undefined)}><Icon name="control-close" size={14} />Close</button>
      </header>
      {#if ofDay?.day === chosen.day}
        <table>
          <thead><tr><th>State</th><th>Run</th><th>Workflow</th><th>Trigger</th><th>By</th><th>Created</th><th class="number">Took</th></tr></thead>
          <tbody>
            {#each ofDay.runs as r (r.run)}
              <tr>
                <td><StatePill state={r.state} /></td>
                <td class="mono"><a href={place.href(runRoute(r))} onclick={follow(place, runRoute(r))}>{r.run}</a></td>
                <td class="mono">{r.namespace}/{r.workflow}</td>
                <td class="mono muted">{r.trigger_kind}</td>
                <td class="mono">{r.triggered_by}</td>
                <td class="mono"><time datetime={r.created_at} title={r.created_at}>{r.created_at.slice(11, 19)} UTC</time></td>
                <td class="mono number">{lasted(r)}</td>
              </tr>
            {:else}
              <tr><td colspan="7" class="muted">No run</td></tr>
            {/each}
          </tbody>
        </table>
        {#if ofDay.more}<p class="faint">First 100 runs</p>{/if}
      {:else}
        <p class="muted" role="status">Reading the day's runs.</p>
      {/if}
    </section>
  {/if}
</Pane>

<div class="columns">
  <Pane title="Namespaces" aside="the last seven days">
    <ul class="namespaces">
      {#each read as n (n.name)}
        {@const s = series.get(n.name)}
        {@const week = s ? lastWeek(s, now) : undefined}
        <li>
          <a class="mono" href={place.href(runsOf(n.name))} onclick={follow(place, runsOf(n.name))}>{n.name}</a>
          <span class="faint">{n.kind}</span>
          {#if week}
            <span class="mono figure">{week.runs} {week.runs === 1 ? "run" : "runs"}</span>
            <span class="mono figure" class:failed={week.failures > 0}>{week.failures} failed</span>
          {:else}
            <span class="faint figure">{reading ? "reading" : "not read"}</span>
          {/if}
        </li>
      {:else}
        <li class="muted">No namespace</li>
      {/each}
    </ul>
  </Pane>
  <Pane title="Last runs" aside="across every namespace">
    <table>
      <thead><tr><th>State</th><th>Run</th><th>Workflow</th><th>Created</th><th class="number">Took</th></tr></thead>
      <tbody>
        {#each recent as r (r.run)}
          <tr>
            <td><StatePill state={r.state} live={r.state === "running"} /></td>
            <td class="mono"><a href={place.href(runRoute(r))} onclick={follow(place, runRoute(r))}>{r.run}</a></td>
            <td class="mono">{r.namespace}/{r.workflow}</td>
            <td class="mono"><time datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time></td>
            <td class="mono number">{lasted(r)}</td>
          </tr>
        {:else}
          <tr><td colspan="5" class="muted">No run</td></tr>
        {/each}
      </tbody>
    </table>
  </Pane>
</div>

<style>
  .hello {
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-sectionTitle-size);
    font-weight: 600;
  }

  .by {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
    margin-left: 34px;
  }

  .tab {
    height: 29px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    font-size: var(--type-navigation-size);
    font-weight: 500;
    cursor: pointer;
  }

  .tab[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .year {
    --square: 16px;
    --gap: 4px;
    --shade: var(--accent);
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 2);
    width: max-content;
    max-width: 100%;
    overflow-x: auto;
    font-size: var(--type-identifier-size-min);
  }

  .year.failures {
    --shade: var(--failed);
  }

  .months {
    display: grid;
    column-gap: var(--gap);
    margin-left: 34px;
    color: var(--faint);
  }

  .body {
    display: flex;
    gap: 6px;
  }

  .weekdays {
    display: grid;
    grid-template-rows: repeat(7, var(--square));
    row-gap: var(--gap);
    width: 28px;
    color: var(--faint);
    line-height: var(--square);
  }

  .squares {
    display: flex;
    gap: var(--gap);
  }

  .week {
    display: grid;
    grid-template-rows: repeat(7, var(--square));
    row-gap: var(--gap);
  }

  .square {
    width: var(--square);
    height: var(--square);
    padding: 0;
    border: var(--border-hairline) solid color-mix(in srgb, var(--text) 6%, transparent);
    border-radius: 2px;
    background: var(--sunken);
    cursor: pointer;
  }

  .square.future {
    border-color: transparent;
    background: none;
    cursor: default;
  }

  .square.chosen,
  .square:focus-visible {
    outline: var(--border-focus) solid var(--text);
    outline-offset: 1px;
  }

  .level-1 {
    background: color-mix(in srgb, var(--shade) 22%, var(--sunken));
  }

  .level-2 {
    background: color-mix(in srgb, var(--shade) 44%, var(--sunken));
  }

  .level-3 {
    background: color-mix(in srgb, var(--shade) 68%, var(--sunken));
  }

  .level-4 {
    background: var(--shade);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: var(--gap);
    margin-top: calc(var(--unit) * 2);
  }



  .legend .square {
    cursor: default;
  }

  .legend .faint {
    margin: 0 calc(var(--unit) * 2);
  }

  .legend .faint.less {
    margin-left: auto;
  }

  .day {
    margin-top: calc(var(--unit) * 7);
  }

  .day header {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 5);
    margin-bottom: calc(var(--unit) * 4);
    font-size: var(--type-navigation-size);
  }

  .day header .control {
    margin-left: auto;
  }

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 2fr);
    align-items: start;
    gap: calc(var(--unit) * 7);
    margin-top: calc(var(--unit) * 8);
  }

  .namespaces {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .namespaces li {
    display: flex;
    align-items: baseline;
    gap: calc(var(--unit) * 4);
    padding: calc(var(--unit) * 3) 0;
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-control-size);
  }

  .namespaces a {
    color: var(--accent);
    font-weight: 600;
  }

  .figure {
    margin-left: auto;
  }

  .figure + .figure {
    margin-left: calc(var(--unit) * 4);
  }

  .figure.failed {
    color: var(--failed);
  }

  .refused {
    margin: 0 0 calc(var(--unit) * 4);
    color: var(--failed);
    font-size: var(--type-control-size);
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    height: var(--row-header);
    padding: 0 calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-align: left;
    text-transform: var(--type-columnHead-case);
  }

  td {
    height: var(--row-body);
    padding: 0 calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-identifier-size-max);
    white-space: nowrap;
  }

  td a {
    color: var(--accent);
  }

  .number {
    text-align: right;
  }
</style>
