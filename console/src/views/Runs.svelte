<script lang="ts">
  import { untrack } from "svelte";
  import type { API, Namespace } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import StepStrip from "../components/StepStrip.svelte";
  import { between, clock, took } from "../lib/format";
  import { follow, type Place } from "../lib/place.svelte";
  import { filtersOf, queryOf, RunList, spans, type Filters, type Run, type RunState, type Span } from "../lib/runs.svelte";

  // The runs view, the screen the console opens on: a namespace's runs, newest first and kept live,
  // the ones that failed lifted into a band above the list, since they are why the page is opened.
  let { api, place, namespace, record }: { api: API; place: Place; namespace: string; record: Namespace | undefined } = $props();

  const filters = $derived(filtersOf(place.query));
  const list = $derived(new RunList(api, namespace, filters));

  let live = $state(true);
  let now = $state(Date.now());

  // Read once for each list, the effect following the list and nothing it reads.
  $effect(() => {
    const l = list;
    untrack(() => l.read());
  });

  // Live, the list is read again every five seconds while the page is in view, and the durations of
  // the runs still going move every second.
  $effect(() => {
    if (!live) {
      return;
    }
    const reading = setInterval(() => {
      if (document.visibilityState === "visible" && !list.reading) {
        list.read();
      }
    }, 5000);
    const ticking = setInterval(() => (now = Date.now()), 1000);
    return () => {
      clearInterval(reading);
      clearInterval(ticking);
    };
  });

  function narrow(change: Partial<Filters>) {
    place.narrow(queryOf({ ...filters, ...change }));
  }

  const bounded = $derived(filters.since && filters.until ? `${filters.since.slice(0, 16).replace("T", " ")} to ${filters.until.slice(11, 16)} UTC` : "");

  // The runs that need somebody: the ones that failed or timed out in the last hour, unless the
  // reader has set them aside.
  let setAside = $state(new Set<string>());
  const attention = $derived(
    list.runs.filter((r) => (r.state === "failed" || r.state === "timed_out") && now - Date.parse(r.created_at) < 3_600_000 && !setAside.has(r.run)),
  );

  // The workflows a reader can narrow to: those of the runs read, and the one the list is narrowed to.
  const workflows = $derived([...new Set([...list.runs.map((r) => r.workflow), ...(filters.workflow ? [filters.workflow] : [])])].sort());

  const chips: { state: RunState | undefined; label: string }[] = [
    { state: undefined, label: "All" },
    { state: "failed", label: "Failed" },
    { state: "running", label: "Running" },
    { state: "queued", label: "Queued" },
    { state: "succeeded", label: "Succeeded" },
    { state: "cancelled", label: "Cancelled" },
    { state: "timed_out", label: "Timed out" },
  ];

  const retention = $derived(record?.quotas?.max_retention_days);

  // opened is the inspector of one run, under the namespace the run is in.
  function opened(r: Run) {
    return { kind: "namespace" as const, namespace: r.namespace, view: "runs" as const, run: r.run };
  }

  function duration(r: Run): string {
    const ms = between(r.started_at, r.finished_at, now);
    return ms === undefined ? "" : took(ms);
  }
</script>

<Pane title="Runs" aside={namespace}>
  <div class="bar">
    <div class="chips" role="group" aria-label="State">
      {#each chips as chip (chip.label)}
        <button class="chip" aria-pressed={filters.state === chip.state} onclick={() => narrow({ state: chip.state })}>{chip.label}</button>
      {/each}
    </div>
    <label class="select">
      <span class="unseen">Workflow</span>
      <select value={filters.workflow ?? ""} onchange={(e) => narrow({ workflow: e.currentTarget.value || undefined })}>
        <option value="">Every workflow</option>
        {#each workflows as w (w)}<option value={w}>{w}</option>{/each}
      </select>
      <Icon name="control-expand" size={14} />
    </label>
    {#if bounded}
      <span class="bounds">
        Created <span class="mono">{bounded}</span>
        <button class="clear" aria-label="Show the last 24 hours again" onclick={() => narrow({ since: undefined, until: undefined, span: "24h" })}><Icon name="control-close" size={12} /></button>
      </span>
    {:else}
      <label class="select">
        <span class="unseen">Created</span>
        <select value={filters.span} onchange={(e) => narrow({ span: e.currentTarget.value as Span })}>
          {#each Object.entries(spans) as [value, span] (value)}<option {value}>{span.label}</option>{/each}
        </select>
        <Icon name="control-expand" size={14} />
      </label>
    {/if}
    <label class="live">
      <input type="checkbox" role="switch" bind:checked={live} />
      <span class="track" aria-hidden="true"><span class="knob"></span></span>
      Live
    </label>
  </div>

  {#if attention.length > 0}
    <section class="attention" aria-label="Needs attention">
      <header>
        <strong>Needs attention</strong>
        <span class="muted">{attention.length === 1 ? "1 run" : `${attention.length} runs`} failed in the last hour</span>
        <button class="link" onclick={() => (setAside = new Set([...setAside, ...attention.map((r) => r.run)]))}>Dismiss all</button>
      </header>
      {#each attention as r (r.run)}
        <div class="failure">
          <StatePill state={r.state} />
          <a class="mono" href={place.href(opened(r))} onclick={follow(place, opened(r))}>{r.run}</a>
          <span class="mono name">{r.workflow}</span>
          <span class="muted">{r.trigger_kind} by <span class="mono">{r.triggered_by}</span></span>
          <time class="muted mono" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time>
        </div>
      {/each}
    </section>
  {/if}

  {#if list.refused}
    <p class="refused" role="alert">The runs could not be read: {list.refused}</p>
  {/if}

  <table>
    <thead>
      <tr>
        <th>State</th>
        <th>Run</th>
        <th>Workflow</th>
        <th>Commit</th>
        <th>Trigger</th>
        <th>Steps</th>
        <th>By</th>
        <th>Created</th>
        <th class="number">Took</th>
      </tr>
    </thead>
    <tbody>
      {#each list.runs as r (r.run)}
        <tr>
          <td><StatePill state={r.state} {live} /></td>
          <td class="mono id"><a href={place.href(opened(r))} onclick={follow(place, opened(r))}>{r.run}</a></td>
          <td class="mono name">{r.workflow}</td>
          <td class="mono muted commit" title={r.commit}>{r.commit.slice(0, 7)}</td>
          <td class="trigger" title={r.from ? `called by run ${r.from.run} at step ${r.from.step}` : undefined}><Icon name="trigger-{r.trigger_kind}" size={14} /><span class="mono">{r.trigger_kind}</span></td>
          <td><StepStrip steps={r.steps ?? []} {now} /></td>
          <td class="mono by">{r.triggered_by}</td>
          <td><time class="mono" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time></td>
          <td class="number mono" class:going={r.state === "running"}>{duration(r)}</td>
        </tr>
      {:else}
        {#if !list.reading && !list.refused}
          <tr><td class="empty" colspan="9">No run {filters.state ? `in ${filters.state} ` : ""}{filters.workflow ? `of ${filters.workflow} ` : ""}was created in this span.</td></tr>
        {/if}
      {/each}
    </tbody>
  </table>

  <footer>
    <span class="muted">
      Showing {list.runs.length === 1 ? "1 run" : `${list.runs.length} runs`}{retention ? ` · retention ${retention} days` : ""}
    </span>
    {#if !list.exhausted && list.runs.length > 0}
      <button class="control" disabled={list.reading} onclick={() => list.more()}>Load 25 more</button>
    {/if}
  </footer>
</Pane>

<style>
  .bar {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 6);
    margin-bottom: calc(var(--unit) * 6);
  }

  .chips {
    display: flex;
    gap: calc(var(--unit) * 3);
  }

  .chip {
    height: 29px;
    padding: 0 calc(var(--unit) * 6);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-round);
    background: var(--raised);
    font-family: var(--type-control-font);
    font-size: var(--type-control-size);
    font-weight: var(--type-control-weight);
    cursor: pointer;
  }

  .chip[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .select {
    position: relative;
    display: inline-flex;
    align-items: center;
    color: var(--muted);
  }

  .select select {
    appearance: none;
    height: 29px;
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

  .bounds {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: 29px;
    padding: 0 calc(var(--unit) * 3) 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-control);
    background: var(--accentDim);
    color: var(--accent);
    font-size: var(--type-control-size);
  }

  .clear {
    display: inline-flex;
    padding: 2px;
    border: none;
    background: none;
    color: inherit;
    cursor: pointer;
  }

  .live {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-left: auto;
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .live input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }

  .track {
    position: relative;
    width: 29px;
    height: 17px;
    border-radius: var(--radius-round);
    background: var(--lineStrong);
  }

  .knob {
    position: absolute;
    top: 2.5px;
    left: 2.5px;
    width: 12px;
    height: 12px;
    border-radius: var(--radius-round);
    background: var(--raised);
    transition: left 0.12s;
  }

  .live input:checked + .track {
    background: var(--accent);
  }

  .live input:checked + .track .knob {
    left: 14.5px;
  }

  .live input:focus-visible + .track {
    outline: var(--border-focus) solid var(--accent);
    outline-offset: 1px;
  }

  .attention {
    margin-bottom: calc(var(--unit) * 6);
    padding: calc(var(--unit) * 5) calc(var(--unit) * 7);
    border-left: 3px solid var(--failed);
    border-radius: var(--radius-innerPanel);
    background: var(--failedFill);
  }

  .attention header {
    display: flex;
    align-items: baseline;
    gap: calc(var(--unit) * 5);
    margin-bottom: calc(var(--unit) * 4);
    font-size: var(--type-navigation-size);
  }

  .attention strong {
    color: var(--failed);
  }

  .link {
    margin-left: auto;
    padding: 0;
    border: none;
    background: none;
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
    cursor: pointer;
  }

  .failure {
    display: grid;
    grid-template-columns: 120px 220px 200px 1fr auto;
    align-items: center;
    gap: calc(var(--unit) * 6);
    padding: calc(var(--unit) * 2) 0;
    font-size: var(--type-identifier-size-max);
  }

  .refused {
    color: var(--failed);
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    height: var(--row-header);
    padding: 0 calc(var(--unit) * 5);
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
    padding: 0 calc(var(--unit) * 5);
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-identifier-size-max);
    white-space: nowrap;
  }

  tbody tr:hover {
    background: var(--raised);
  }

  .number {
    text-align: right;
  }

  .name {
    font-weight: 600;
  }

  .id {
    font-size: var(--type-identifier-size-min);
  }

  .id a {
    color: var(--muted);
  }

  .id a:hover {
    color: var(--accent);
  }

  .trigger {
    color: var(--muted);
  }

  .trigger :global(.icon) {
    vertical-align: -2px;
    margin-right: calc(var(--unit) * 3);
  }

  .by {
    color: var(--muted);
  }

  .going {
    color: var(--running);
  }

  .empty {
    height: 96px;
    color: var(--muted);
    text-align: center;
  }

  footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-top: calc(var(--unit) * 6);
  }
</style>
