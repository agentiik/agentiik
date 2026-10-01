<script lang="ts">
  import { untrack } from "svelte";
  import { refusal, type API, type Me } from "../api/client";
  import type { components } from "../api/schema";
  import FileView from "../components/FileView.svelte";
  import GraphCanvas from "../components/GraphCanvas.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import StepDetail from "../components/StepDetail.svelte";
  import Refused from "./Refused.svelte";
  import { clock, took } from "../lib/format";
  import { authOf, layout, triggers } from "../lib/graph";
  import { follow, type Place } from "../lib/place.svelte";
  import { RunReader } from "../lib/run.svelte";
  import { blocks } from "../lib/yaml-blocks";

  // A workflow: the version a run naming no ref runs, the head of its default branch, with what
  // starts it and how its runs queue, the history behind it, and its graph as that version resolved
  // it, with the state of a run laid over it: the one the address names, or the workflow's latest,
  // read again while it runs. Beside the graph, the step chosen as it resolved, or the file it was
  // written in, each selecting the other.
  let { api, place, me, namespace, workflow }: { api: API; place: Place; me: Me; namespace: string; workflow: string } = $props();

  type Detail = components["schemas"]["workflowDetail"];
  type Entry = components["schemas"]["historyEntry"];

  let detail = $state<Detail | null>(null);
  let missing = $state(false);
  let refused = $state("");
  let text = $state<string | null>(null);
  let history = $state<Entry[]>([]);
  let next = $state<string | undefined>(undefined);
  let latest = $state<string | undefined>(undefined);

  async function read() {
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } } });
    if (!data) {
      if (response.status === 404) missing = true;
      else refused = refusal(response, error).message;
      return;
    }
    detail = data;
    history = data.history;
    next = data.next;
    const commit = data.version?.commit;
    if (commit) {
      const file = await api.GET("/api/v1/{ns}/workflows/{name}/tree/{ref}", { params: { path: { ns: namespace, name: workflow, ref: commit }, query: { path: "agentiik.yaml" } }, parseAs: "text" });
      text = typeof file.data === "string" ? file.data : null;
    }
    const runs = await api.GET("/api/v1/runs", { params: { query: { namespace, workflow, limit: 1 } } });
    latest = runs.data?.runs.find((r) => r.workflow === workflow)?.run;
  }

  $effect(() => {
    untrack(() => read());
  });

  async function more() {
    if (!next) return;
    const { data } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow }, query: { from: next } } });
    if (data) {
      history = [...history, ...data.history];
      next = data.next;
    }
  }

  // The run laid over the graph, read again every five seconds while it has not ended.
  const shownRun = $derived(place.query.get("run") ?? latest);
  const reader = $derived(shownRun ? new RunReader(api, shownRun) : null);
  $effect(() => {
    const r = reader;
    if (!r) return;
    untrack(() => r.read());
    const reading = setInterval(() => {
      if (!r.ended && document.visibilityState === "visible") r.read();
    }, 5000);
    return () => clearInterval(reading);
  });
  const run = $derived(reader?.run && reader.run.workflow === workflow && reader.run.namespace === namespace ? reader.run : null);

  const graph = $derived(detail?.graph);
  const on = $derived(graph ? triggers(graph) : { schedule: [], webhook: [], event: [] });
  const laid = $derived(graph ? layout(graph) : null);
  const found = $derived(text ? blocks(text) : new Map());
  const selected = $derived.by(() => {
    const named = place.query.get("step");
    return graph && named && named in graph.steps ? named : graph?.order[0];
  });
  const pane = $derived(place.query.get("pane") === "file" ? "file" : "step");
  let showHistory = $state(false);

  function narrow(set: Record<string, string>) {
    const q = new URLSearchParams(place.query);
    for (const [k, v] of Object.entries(set)) q.set(k, v);
    place.narrow(q);
  }

  const runsOf = $derived({ kind: "namespace" as const, namespace, view: "runs" as const });
  const statistics = $derived({ kind: "namespace" as const, namespace, view: "workflows" as const, workflow, tab: "statistics" });
  const runRoute = $derived(run ? { kind: "namespace" as const, namespace, view: "runs" as const, run: run.run } : undefined);
  const now = Date.now();
</script>

{#if missing}
  <Refused />
{:else if refused}
  <Pane title={workflow}><p class="refused" role="alert">The workflow could not be read: {refused}</p></Pane>
{:else if detail}
  <nav class="sub" aria-label="{namespace}/{workflow}">
    <span class="mono where">{namespace} / {workflow}</span>
    <span class="tab" aria-current="page">Graph</span>
    <a class="tab" href={place.href(runsOf) + `?workflow=${encodeURIComponent(workflow)}`} onclick={(e) => { if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; e.preventDefault(); place.go(runsOf, false, new URLSearchParams({ workflow })); }}>Runs</a>
    <a class="tab" href={place.href(statistics)} onclick={follow(place, statistics)}>Statistics</a>
    <span class="right">
      {#if detail.version}
        <span class="version mono" title={detail.version.commit}>{detail.repository.default_branch} · {detail.version.commit.slice(0, 7)}</span>
      {/if}
      <button class="control" aria-pressed={showHistory} onclick={() => (showHistory = !showHistory)}><Icon name="control-history" size={14} />History</button>
    </span>
  </nav>

  <section class="about" aria-label="What starts it">
    {#if detail.version}
      <span class="muted">Head of <span class="mono">{detail.repository.default_branch}</span>, pushed by <span class="mono">{detail.version.author}</span> <time datetime={detail.version.created_at} title={detail.version.created_at}>{clock(detail.version.created_at, now)}</time>{detail.repository.protected ? "; the branch is protected" : ""}.</span>
    {:else}
      <span class="muted">No version yet: the repository holds nothing a run could run.</span>
    {/if}
    {#if graph}
      <ul class="triggers">
        {#each on.schedule as t, i (i)}
          <li><Icon name="trigger-schedule" size={14} /><span class="mono">{t.cron}</span>{#if t.timezone}<span class="muted">{t.timezone}</span>{/if}{#if t.jitter}<span class="muted">jitter {t.jitter}</span>{/if}{#if t.catch_up !== undefined}<span class="muted">catch_up {String(t.catch_up)}</span>{/if}</li>
        {/each}
        {#each on.webhook as t, i (i)}
          <li><Icon name="trigger-webhook" size={14} /><span class="mono">{t.method ?? "POST"} /hooks/{namespace}/{t.path}</span><span class="muted">auth {authOf(t)}, response {t.response ?? "async"}</span></li>
        {/each}
        {#each on.event as t, i (i)}
          <li><Icon name="trigger-event" size={14} /><span class="mono">{t.type}</span>{#if t.source}<span class="muted">from <span class="mono">{t.source}</span></span>{/if}{#if t.namespace}<span class="muted">in <span class="mono">{t.namespace}</span></span>{/if}{#if t.filter}<span class="muted" title={t.filter}>filtered</span>{/if}</li>
        {/each}
        {#if !on.schedule.length && !on.webhook.length && !on.event.length}
          <li class="muted">No schedule, webhook or event starts it: it runs when asked.</li>
        {/if}
        {#if graph.concurrency}
          <li><span class="muted">concurrency</span><span class="mono">{graph.concurrency.group}</span><span class="muted">{graph.concurrency.cancel_in_progress ? "an arriving run cancels the one going" : "a run waits for the one going"}</span></li>
        {/if}
        {#if graph.timeout}<li><span class="muted">timeout</span><span class="mono">{graph.timeout}</span></li>{/if}
        {#if graph.retain}<li><span class="muted">retain</span><span class="mono">{graph.retain}</span></li>{/if}
      </ul>
    {/if}
  </section>

  {#if showHistory}
    <Pane title="History" aside="the first-parent history of {detail.repository.default_branch}">
      <table class="history">
        <thead><tr><th>Commit</th><th>Subject</th><th>Author</th><th>When</th><th>Version</th></tr></thead>
        <tbody>
          {#each history as h (h.commit)}
            <tr>
              <td class="mono" title={h.commit}>{h.commit.slice(0, 7)}</td>
              <td>{h.subject ?? ""}</td>
              <td>{h.author?.name ?? ""}</td>
              <td class="mono"><time datetime={h.authored_at}>{h.authored_at ? clock(h.authored_at, now) : ""}</time></td>
              <td class="muted">{h.version ? "a version" : "not a version"}</td>
            </tr>
          {:else}
            <tr><td colspan="5" class="muted">Nothing has been pushed yet.</td></tr>
          {/each}
        </tbody>
      </table>
      {#if next}<button class="control more" onclick={more}>Older commits</button>{/if}
    </Pane>
  {/if}

  {#if graph && laid}
    <div class="columns">
      <Pane title="Graph" aside={run ? `run ${run.run}` : "no run yet"} focused>
        {#if run && runRoute}
          <div class="run">
            <StatePill state={run.state} live={!reader?.ended} />
            <a class="mono" href={place.href(runRoute)} onclick={follow(place, runRoute)}>{run.run}</a>
            <span class="muted"><span class="mono">{run.trigger_kind}</span> · by <span class="mono">{run.triggered_by}</span>{#if run.started_at} · started <time class="mono" datetime={run.started_at}>{clock(run.started_at, now)}</time> · <span class="mono">{took(Math.max(0, (run.finished_at ? Date.parse(run.finished_at) : Date.now()) - Date.parse(run.started_at)))}</span>{/if}</span>
            {#if run.commit !== detail.version?.commit}<span class="muted">of an older version, <span class="mono">{run.commit.slice(0, 7)}</span>, drawn on this one</span>{/if}
          </div>
        {/if}
        <GraphCanvas {graph} {laid} {run} {selected} onselect={(step) => narrow({ step })} />
      </Pane>
      <Pane title={pane === "file" ? "agentiik.yaml" : (selected ?? "Step")} aside={pane === "file" ? `${detail.repository.default_branch} · ${detail.version?.commit.slice(0, 7) ?? ""}` : graph.steps[selected ?? ""]?.kind}>
        <div class="tabs" role="group" aria-label="Shown beside the graph">
          <button class="tab" aria-pressed={pane === "step"} onclick={() => narrow({ pane: "step" })}>Step</button>
          <button class="tab" aria-pressed={pane === "file"} onclick={() => narrow({ pane: "file" })}>agentiik.yaml</button>
        </div>
        {#if pane === "file"}
          {#if text !== null}
            <FileView {text} {found} {selected} onselect={(step) => narrow({ step })} />
            {#if selected && !found.has(selected)}<p class="faint">{selected} is not written in this file: it comes from a file this one includes.</p>{/if}
          {:else}
            <p class="muted">The file could not be read.</p>
          {/if}
        {:else if selected && graph.steps[selected]}
          <StepDetail name={selected} step={graph.steps[selected]!} {run} {place} {namespace} />
        {/if}
      </Pane>
    </div>
  {:else}
    <Pane title="Graph"><p class="muted">This workflow has no version to draw, or its version is a library, which no run runs.</p></Pane>
  {/if}
{/if}

<style>
  .sub {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
    margin: calc(var(--unit) * -3) 0 calc(var(--unit) * 5);
  }

  .where {
    margin-right: calc(var(--unit) * 6);
    font-weight: 600;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    height: 29px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid transparent;
    border-radius: var(--radius-control);
    background: none;
    color: var(--muted);
    font-size: var(--type-navigation-size);
    font-weight: 500;
    text-decoration: none;
    cursor: pointer;
  }

  .tab[aria-current="page"],
  .tab[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .right {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-left: auto;
  }

  .version {
    padding: 3px calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    font-size: var(--type-identifier-size-min);
  }

  .about {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 3);
    margin-bottom: calc(var(--unit) * 8);
    font-size: var(--type-control-size);
  }

  .triggers {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .triggers li {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .history {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  .history th,
  .history td {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
    text-align: left;
  }

  .history th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .more {
    margin-top: calc(var(--unit) * 5);
  }

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 1.55fr) minmax(0, 1fr);
    gap: calc(var(--unit) * 7);
    margin-top: calc(var(--unit) * 8);
  }

  .run {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-bottom: calc(var(--unit) * 5);
    font-size: var(--type-control-size);
  }

  .run a {
    color: var(--accent);
  }

  .tabs {
    display: flex;
    gap: calc(var(--unit) * 2);
    margin-bottom: calc(var(--unit) * 6);
  }

  .refused {
    color: var(--failed);
  }
</style>
