<script lang="ts">
  import Problem from "../components/Problem.svelte";
  import { untrack } from "svelte";
  import type { API, Me, Namespace } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import StepStrip from "../components/StepStrip.svelte";
  import { between, clock, took } from "../lib/format";
  import { moved, useKeys } from "../lib/keys.svelte";
  import { useLive } from "../lib/live.svelte";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import { filtersOf, queryOf, RunList, spans, type Filters, type Run, type RunState, type Span } from "../lib/runs.svelte";
  import { workflowTabs } from "../lib/page";
  import { runAt } from "../lib/route";

  // A workflow's runs, a tab of the workflow: newest first and kept live, the ones that failed lifted
  // into a band above the list, since they are why the page is opened. A run is always some
  // workflow's, so its runs are listed with it rather than in a list of the namespace's.
  // graph says whether the caller reads the workflow itself, which its graph takes and its runs do
  // not: the tab is left out for one who reads only its runs.
  let { api, place, me, namespace, workflow, record }: { api: API; place: Place; me: Me; namespace: string; workflow: string; record: Namespace | undefined } = $props();

  const filters = $derived({ ...filtersOf(place.query), workflow });
  const graph = $derived(holds(me, "workflow:read", namespace, workflow));
  const tabs = $derived(workflowTabs(namespace, workflow, "runs", { shares: holds(me, "grant:manage", namespace, workflow), mcp: false, go: (r, q) => place.go(r, false, q) }).filter((t) => graph || t.label !== "Graph"));
  const list = $derived(new RunList(api, namespace, filters));

  let live = $state(true);
  let now = $state(Date.now());

  // Read once for each list, the effect following the list and nothing it reads.
  $effect(() => {
    const l = list;
    untrack(() => l.read());
  });

  // Live, the list is read again each time the live connection says a run it may list changed, and
  // the durations of the runs still going move every second.
  const changes = useLive();
  $effect(() => {
    if (!live) {
      return;
    }
    const l = list;
    const reading = changes.when((c) => c.kind === "run" && c.namespace === namespace && c.workflow === workflow, () => {
      if (!l.reading) l.read();
    });
    const ticking = setInterval(() => (now = Date.now()), 1000);
    return () => {
      reading();
      clearInterval(ticking);
    };
  });

  function narrow(change: Partial<Filters>) {
    place.narrow(queryOf({ ...filters, ...change, workflow: undefined }));
  }

  const bounded = $derived(filters.since && filters.until ? `${filters.since.slice(0, 16).replace("T", " ")} to ${filters.until.slice(11, 16)} UTC` : "");

  // The runs that need somebody: the ones that failed or timed out in the last hour, unless the
  // reader has set them aside.
  let setAside = $state(new Set<string>());
  const attention = $derived(
    list.runs.filter((r) => (r.state === "failed" || r.state === "timed_out") && now - Date.parse(r.created_at) < 3_600_000 && !setAside.has(r.run)),
  );

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

  // opened is the inspector of one run, under its workflow.
  function opened(r: Run) {
    return runAt(r.namespace, r.workflow, r.run);
  }

  // The run selected with the keys, by its identifier, so that a list read again keeps it.
  let selected = $state<string | undefined>(undefined);
  let body = $state<HTMLElement | undefined>(undefined);

  function select(run: string | undefined) {
    selected = run;
    if (run) body?.querySelector(`[data-run="${run}"]`)?.scrollIntoView?.({ block: "nearest" });
  }

  useKeys(() =>
    list.runs.length === 0
      ? []
      : [
          { keys: ["ArrowUp", "ArrowDown", "k", "j"], brief: ["ArrowUp", "ArrowDown"], effect: "Move", does: (key) => select(moved(list.runs.map((r) => r.run), selected, key)) },
          {
            keys: ["Enter"],
            effect: "Open",
            does: () => {
              const r = list.runs.find((r) => r.run === selected);
              if (r) place.go(opened(r));
            },
          },
        ],
  );

  function duration(r: Run): string {
    const ms = between(r.started_at, r.finished_at, now);
    return ms === undefined ? "" : took(ms);
  }
</script>

<PageHeader title={workflow} icon="control-workflows" {place} {tabs}>
  {#snippet actions()}
    <label class="live">
      <input type="checkbox" role="switch" bind:checked={live} />
      <span class="track" aria-hidden="true"><span class="knob"></span></span>
      Live
    </label>
  {/snippet}
</PageHeader>

<Pane title="" label="Runs of {workflow}">
  <div class="bar">
    <div class="chips" role="group" aria-label="State">
      {#each chips as chip (chip.label)}
        <button class="chip" aria-pressed={filters.state === chip.state} onclick={() => narrow({ state: chip.state })}>{chip.label}</button>
      {/each}
    </div>
    {#if bounded}
      <span class="bounds">
        Created <span class="term">{bounded}</span>
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
          <a class="code" href={place.href(opened(r))} onclick={follow(place, opened(r))}>{r.run}</a>
          <span class="muted">{r.trigger_kind} by <span class="term">{r.triggered_by}</span></span>
          <time class="muted term" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time>
        </div>
      {/each}
    </section>
  {/if}

  {#if list.refused}
    <Problem explained={list.refused} onretry={() => list.read()} />
  {/if}

  <div class="scroll">
  <table>
    <thead>
      <tr>
        <th>State</th>
        <th>Run</th>
        <th>Commit</th>
        <th>Trigger</th>
        <th>Steps</th>
        <th>By</th>
        <th>Created</th>
        <th class="number">Took</th>
      </tr>
    </thead>
    <tbody bind:this={body}>
      {#each list.runs as r (r.run)}
        <tr data-run={r.run} class:chosen={r.run === selected} aria-selected={r.run === selected} onclick={() => (selected = r.run)}>
          <td><StatePill state={r.state} {live} /></td>
          <td class="code id"><a href={place.href(opened(r))} onclick={follow(place, opened(r))}>{r.run}</a></td>
          <td class="code muted commit" title={r.commit}>{r.commit.slice(0, 7)}</td>
          <td class="trigger" title={r.from ? `called by run ${r.from.run} at step ${r.from.step}` : undefined}><Icon name="trigger-{r.trigger_kind}" size={14} /><span class="term">{r.trigger_kind}</span></td>
          <td><StepStrip steps={r.steps ?? []} {now} /></td>
          <td class="term by">{r.triggered_by}</td>
          <td><time class="term" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time></td>
          <td class="number term" class:going={r.state === "running"}>{duration(r)}</td>
        </tr>
      {:else}
        {#if !list.reading && !list.refused}
          <tr><td class="empty" colspan="8">No runs</td></tr>
        {/if}
      {/each}
    </tbody>
  </table>
  </div>

  {#if list.settled}
  <footer>
    <span class="muted">
      Showing {list.runs.length === 1 ? "1 run" : `${list.runs.length} runs`}{retention ? ` · retention ${retention} days` : ""}
    </span>
    {#if !list.exhausted && list.runs.length > 0}
      <button class="control" disabled={list.reading} onclick={() => list.more()}>Load 25 more</button>
    {/if}
  </footer>
  {/if}
</Pane>

<style>
  /* The filters wrap onto a second line where the window is too narrow for one. */
  .bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4) calc(var(--unit) * 6);
    margin-bottom: calc(var(--unit) * 6);
  }

  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3);
  }

  .chip {
    height: var(--control-height);
    white-space: nowrap;
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

  .bounds {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    height: var(--control-height);
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
    width: 30px;
    height: 18px;
    border-radius: var(--radius-round);
    background: var(--lineStrong);
  }

  .knob {
    position: absolute;
    top: 3px;
    left: 3px;
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
    left: 15px;
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
    overflow-x: auto;
    align-items: center;
    gap: calc(var(--unit) * 6);
    padding: calc(var(--unit) * 2) 0;
    font-size: var(--type-identifier-size-max);
  }


  /* The table alone scrolls where the window is narrower than its columns, the filters above it
     and the count under it staying put. */
  .scroll {
    overflow-x: auto;
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    height: var(--row-header);
    padding: 0 calc(var(--unit) * 5);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
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
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    font-size: var(--type-identifier-size-max);
    white-space: nowrap;
  }

  tbody tr:hover,
  tbody tr.chosen {
    background: var(--raised);
  }

  tbody tr.chosen td:first-child {
    box-shadow: inset 3px 0 0 var(--accent), inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
  }

  .number {
    text-align: right;
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
