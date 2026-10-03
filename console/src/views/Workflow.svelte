<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { untrack } from "svelte";
  import { refusal, type API, type Me, type Namespace } from "../api/client";
  import type { components } from "../api/schema";
  import FileView from "../components/FileView.svelte";
  import Files from "../components/Files.svelte";
  import GraphCanvas from "../components/GraphCanvas.svelte";
  import Icon from "../components/Icon.svelte";
  import McpPanel from "../components/McpPanel.svelte";
  import Notice from "../components/Notice.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Pane from "../components/Pane.svelte";
  import RunForm from "../components/RunForm.svelte";
  import StatePill from "../components/StatePill.svelte";
  import StepDetail from "../components/StepDetail.svelte";
  import WorkflowSettings from "../components/WorkflowSettings.svelte";
  import Refused from "./Refused.svelte";
  import { clock, took } from "../lib/format";
  import { authOf, layout, triggers, type Graph } from "../lib/graph";
  import { starting } from "../lib/editor/starting";
  import { moved, useKeys } from "../lib/keys.svelte";
  import { useLive } from "../lib/live.svelte";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import { runAt, runsOf } from "../lib/route";
  import { RunReader } from "../lib/run.svelte";
  import { settles as settlesOn, workflowTabs } from "../lib/page";
  import { blocks } from "../lib/yaml-blocks";

  // A workflow: the version a run naming no ref runs, the head of its default branch, with what
  // starts it and how its runs queue, the history behind it, and its graph as that version resolved
  // it, with the state of a run laid over it: the one the address names, or the workflow's latest,
  // read again while it runs. Beside the graph, the step chosen as it resolved, or the file it was
  // written in, each selecting the other.
  // tab is the page's: its graph where it names none, its files, the tools it publishes, or its
  // settings, which a move offers the namespaces the caller reads as places to move it to.
  let {
    api,
    place,
    me,
    namespace,
    workflow,
    tab,
    namespaces = [],
  }: { api: API; place: Place; me: Me; namespace: string; workflow: string; tab?: string; namespaces?: Namespace[] } = $props();

  type Detail = components["schemas"]["workflowDetail"];
  type Entry = components["schemas"]["historyEntry"];

  let detail = $state<Detail | null>(null);
  let missing = $state(false);
  let refused = $state<Explained | null>(null);
  let text = $state<string | null>(null);
  let history = $state<Entry[]>([]);
  let next = $state<string | undefined>(undefined);
  let latest = $state<string | undefined>(undefined);

  async function read() {
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } } });
    if (!data) {
      if (response.status === 404) missing = true;
      else refused = explain("load the workflow", refusal(response, error));
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
    await readLatest();
  }

  // The workflow's latest run, which the graph shows where the address names none.
  async function readLatest() {
    const runs = await api.GET("/api/v1/runs", { params: { query: { namespace, workflow, limit: 1 } } });
    latest = runs.data?.runs.find((r) => r.workflow === workflow)?.run;
  }

  $effect(() => {
    untrack(() => read());
  });

  // A run of the workflow started or moved on, as the live connection says: which is its latest is
  // read again, and the run laid over the graph below.
  const changes = useLive();
  $effect(() => {
    const ns = namespace;
    const name = workflow;
    return changes.when((c) => c.kind === "run" && c.namespace === ns && c.workflow === name, () => readLatest());
  });

  async function more() {
    if (!next) return;
    const { data } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow }, query: { from: next } } });
    if (data) {
      history = [...history, ...data.history];
      next = data.next;
    }
  }

  // The run laid over the graph, read again each time the live connection says it changed.
  // None on the files or the tools, which draw no run.
  const shownRun = $derived(tab === "files" || tab === "mcp" ? undefined : (place.query.get("run") ?? latest));
  const reader = $derived(shownRun ? new RunReader(api, shownRun) : null);
  $effect(() => {
    const r = reader;
    const id = shownRun;
    if (!r) return;
    untrack(() => r.read());
    return changes.when((c) => c.kind === "run" && c.run === id, () => r.read());
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
  // Starting a run is workflow:run's, which the button is left out without.
  const mayRun = $derived(holds(me, "workflow:run", namespace, workflow));
  let running = $state(false);
  // The ref a run is asked for at, the head where the run form opens from the page's own button, and
  // the ref the files are read at where it opens from theirs, so that a branch is tried on real
  // inputs before it is merged.
  let runRef = $state("");
  function runFrom(ref: string) {
    runRef = ref === detail?.repository.default_branch ? "" : ref;
    running = true;
  }

  function narrow(set: Record<string, string | null>) {
    const q = new URLSearchParams(place.query);
    for (const [k, v] of Object.entries(set)) {
      if (v === null) q.delete(k);
      else q.set(k, v);
    }
    place.narrow(q);
  }

  // A commit from the editor: on the default branch, the page reads the workflow again at its new
  // head; on a new branch, its files are opened at that branch, where Run tries it.
  let said = $state("");
  async function committed(branch: string, _commit: string) {
    if (detail && branch === detail.repository.default_branch) {
      narrow({ edit: null, view: null });
      await read();
      said = `Committed to ${branch}.`;
      return;
    }
    place.go({ kind: "namespace", namespace, view: "workflows", workflow, tab: "files" }, false, new URLSearchParams({ ref: branch }));
  }

  // The visual editor, under workflow:write, over the file at the head of the default branch: the
  // address says it is open, so that Back leaves it.
  const mayEdit = $derived(holds(me, "workflow:write", namespace, workflow));
  const editing = $derived(place.query.get("edit") === "1" && mayEdit && (tab === undefined || tab === "graph" || tab === "files"));

  // An empty repository is written from here too: the editor opens on a first agentiik.yaml naming
  // the workflow, and its commit is the repository's first, which POST .../commits makes with no
  // parent. Its image is pinned by digest, since the hook refuses a tag nobody pinned and pinning one
  // takes agk push, where the image is.
  const first = $derived(starting(namespace, workflow));
  const empty = $derived(detail?.repository.head === null && !detail?.version);
  const nothing: Graph = { workflow: "", commit: "", includes: [], order: [], steps: {} } as unknown as Graph;
  let editSelected = $state<string | undefined>(untrack(() => place.query.get("step") ?? undefined));

  // The steps in the order the graph draws them, row by row and left to right, which the keys move
  // along as a reader's eye does.
  const drawn = $derived(laid ? [...laid.nodes].sort((a, b) => a.layer - b.layer || a.x - b.x).map((n) => n.step) : []);

  useKeys(() =>
    drawn.length === 0
      ? []
      : [
          {
            keys: ["ArrowUp", "ArrowDown", "k", "j"],
            brief: ["ArrowUp", "ArrowDown"],
            effect: "Step",
            does: (key) => {
              const step = moved(drawn, selected, key);
              if (step) narrow({ step });
            },
          },
          { keys: ["Escape"], effect: "Its runs", does: () => place.go(runsOf(namespace, workflow)) },
        ],
  );

  const shares = $derived(holds(me, "grant:manage", namespace, workflow));
  const statistics = $derived({ kind: "namespace" as const, namespace, view: "workflows" as const, workflow, tab: "statistics" });
  const graphTab = $derived({ kind: "namespace" as const, namespace, view: "workflows" as const, workflow });
  const mcpTab = $derived({ kind: "namespace" as const, namespace, view: "workflows" as const, workflow, tab: "mcp" });
  const filesTab = $derived({ kind: "namespace" as const, namespace, view: "workflows" as const, workflow, tab: "files" });
  const runRoute = $derived(run ? runAt(namespace, workflow, run.run) : undefined);
  // Its settings, to a caller who may change any of them.
  const settles = $derived(settlesOn(me, namespace, workflow));
  const tabs = $derived(workflowTabs(namespace, workflow, tab, { mcp: !!graph?.mcp, settles }));
  const now = Date.now();
</script>

{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<PageHeader title={workflow} icon="control-workflows" {place} tabs={tabs}>
  {#snippet subtitle()}
    {#if detail?.version}<span class="version" title={detail.version.commit}>{detail.repository.default_branch} · <span class="code">{detail.version.commit.slice(0, 7)}</span></span>{/if}
  {/snippet}
  {#snippet actions()}
    {#if detail}
      <button class="control" aria-pressed={showHistory} onclick={() => (showHistory = !showHistory)}><Icon name="control-history" size={14} />History</button>
      {#if mayEdit && graph && detail.version && text !== null && !editing && (tab === undefined || tab === "graph")}
        <button class="control" onclick={() => { editSelected = selected; narrow({ edit: "1" }); }}><Icon name="control-edit" size={14} />Edit</button>
      {/if}
      {#if mayRun && graph && detail.version}
        <button class="control primary" aria-pressed={running} onclick={() => { runRef = ""; running = !running; }}><Icon name="control-run" size={14} />Run</button>
      {/if}
    {/if}
  {/snippet}
</PageHeader>

{#if missing}
  <Refused />
{:else if refused}
  <Pane title={workflow}><Problem explained={refused} onretry={read} /></Pane>
{:else if detail}

  <section class="about" aria-label="What starts it">
    {#if detail.version}
      <span class="muted"><span class="term">{detail.repository.default_branch}</span>{detail.repository.protected ? " (protected)" : ""} · <span class="term">{detail.version.author}</span> · <time datetime={detail.version.created_at} title={detail.version.created_at}>{clock(detail.version.created_at, now)}</time></span>
    {:else}
      <span class="muted">Nothing pushed yet</span>
    {/if}
    {#if graph}
      <ul class="triggers">
        {#each on.schedule as t, i (i)}
          <li><Icon name="trigger-schedule" size={14} /><span class="code">{t.cron}</span>{#if t.timezone}<span class="muted">{t.timezone}</span>{/if}{#if t.jitter}<span class="muted">jitter {t.jitter}</span>{/if}{#if t.catch_up !== undefined}<span class="muted">catch_up {String(t.catch_up)}</span>{/if}</li>
        {/each}
        {#each on.webhook as t, i (i)}
          <li><Icon name="trigger-webhook" size={14} /><span class="code">{t.method ?? "POST"} /hooks/{namespace}{t.path}</span><span class="muted">auth {authOf(t)}, response {t.response ?? "async"}</span></li>
        {/each}
        {#each on.event as t, i (i)}
          <li><Icon name="trigger-event" size={14} /><span class="term">{t.type}</span>{#if t.source}<span class="muted">from <span class="term">{t.source}</span></span>{/if}{#if t.namespace}<span class="muted">in <span class="term">{t.namespace}</span></span>{/if}{#if t.filter}<span class="muted" title={t.filter}>filtered</span>{/if}</li>
        {/each}
        {#if !on.schedule.length && !on.webhook.length && !on.event.length}
          <li class="muted">Manual only</li>
        {/if}
        {#if graph.concurrency}
          <li><span class="muted">concurrency</span><span class="term">{graph.concurrency.group}</span>{#if graph.concurrency.cancel_in_progress}<span class="muted">cancel_in_progress</span>{/if}</li>
        {/if}
        {#if graph.timeout}<li><span class="muted">timeout</span><span class="term">{graph.timeout}</span></li>{/if}
        {#if graph.retain}<li><span class="muted">retain</span><span class="term">{graph.retain}</span></li>{/if}
      </ul>
    {/if}
  </section>

  {#if showHistory}
    <Pane title="History" aside={detail.repository.default_branch}>
      <table class="history">
        <thead><tr><th>Commit</th><th>Subject</th><th>Author</th><th>When</th><th>Version</th></tr></thead>
        <tbody>
          {#each history as h (h.commit)}
            <tr>
              <td class="code" title={h.commit}>{h.commit.slice(0, 7)}</td>
              <td>{h.subject ?? ""}</td>
              <td>{h.author?.name ?? ""}</td>
              <td class="term"><time datetime={h.authored_at}>{h.authored_at ? clock(h.authored_at, now) : ""}</time></td>
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

  {#if running && graph && detail.version}
    <div class="runform">
      <Pane title="Run {workflow}">
        {#key runRef}
          <RunForm {api} {place} {namespace} {workflow} ref={runRef} onclose={() => (running = false)} />
        {/key}
      </Pane>
    </div>
  {/if}

  {#if editing && empty}
    {#await import("../components/Editor.svelte") then { default: Editor }}
      <Editor {api} {namespace} {workflow} commit={null} entry={first} base={nothing} branch={detail.repository.default_branch} ontoDefault={true} layout={place.query.get("view") === "yaml" ? "yaml" : "graph"} bind:selected={editSelected} onlayout={(l) => narrow({ view: l === "yaml" ? "yaml" : null })} onclose={() => narrow({ edit: null, view: null })} oncommitted={committed} />
    {/await}
  {:else if (tab === "files" || tab === undefined || tab === "graph") && empty}
    <Pane title="Files">
      <p class="muted">Empty repository</p>
      {#if mayEdit}
        <p><button class="control primary" onclick={() => narrow({ edit: "1" })}><Icon name="control-edit" size={14} />Write agentiik.yaml</button></p>
      {/if}
      <pre class="clone term">git clone {detail.repository.clone_url}</pre>
    </Pane>
  {:else if tab === "files"}
    <Files {api} {place} {namespace} {workflow} repository={detail.repository} {history} {mayRun} onrun={runFrom} />
  {:else if tab === "settings" && settles}
    <WorkflowSettings {api} {place} {me} {namespace} {workflow} repository={detail.repository} {namespaces} />
  {:else if tab === "settings"}
    <Refused />
  {:else if tab === "mcp" && graph}
    <Pane title="MCP">
      <McpPanel {graph} {workflow} />
    </Pane>
  {:else if editing && graph && detail.version && text !== null}
    {#await import("../components/Editor.svelte") then { default: Editor }}
      <Editor {api} {namespace} {workflow} commit={detail.version.commit} entry={text} base={graph} branch={detail.repository.default_branch} ontoDefault={!detail.repository.protected || shares} layout={place.query.get("view") === "yaml" ? "yaml" : "graph"} bind:selected={editSelected} onlayout={(l) => narrow({ view: l === "yaml" ? "yaml" : null })} onclose={() => narrow({ edit: null, view: null })} oncommitted={committed} />
    {/await}
  {:else if graph && laid}
    <div class="columns fills">
      <Pane title="Graph" aside={run ? "last run" : ""} focused>
        <!-- The run's line holds its height before the run is read, so that the canvas under it stays put. -->
        <div class="run">
          {#if run && runRoute}
            <StatePill state={run.state} live={!reader?.ended} />
            <a class="code" href={place.href(runRoute)} onclick={follow(place, runRoute)}>{run.run}</a>
            <span class="muted info"><span class="term">{run.trigger_kind}</span> · by <span class="term">{run.triggered_by}</span>{#if run.started_at} · started <time class="term" datetime={run.started_at}>{clock(run.started_at, now)}</time> · <span class="term">{took(Math.max(0, (run.finished_at ? Date.parse(run.finished_at) : Date.now()) - Date.parse(run.started_at)))}</span>{/if}{#if run.commit !== detail.version?.commit} · older version <span class="code">{run.commit.slice(0, 7)}</span>{/if}</span>
          {/if}
        </div>
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
            {#if selected && !found.has(selected)}<p class="faint">{selected} is in an included file.</p>{/if}
          {:else}
            <p class="muted">The file could not be read.</p>
          {/if}
        {:else if selected && graph.steps[selected]}
          <StepDetail name={selected} step={graph.steps[selected]!} {run} {place} {namespace} />
        {/if}
      </Pane>
    </div>
  {:else}
    <Pane title="Graph"><p class="muted">No graph</p></Pane>
  {/if}
{/if}

<style>

  .tab {
    display: inline-flex;
    align-items: center;
    height: var(--control-height);
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

  .tab[aria-pressed="true"] {
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  .version {
    padding: 3px calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    font-size: var(--type-identifier-size-min);
  }

  /* What starts the workflow, on one card above its graph: the version drawn, then each trigger and
     each bound on a chip of its own, so that a long webhook path wraps as a whole rather than mid-way. */
  .about {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 4);
    margin-bottom: calc(var(--unit) * 8);
    padding: calc(var(--unit) * 5) var(--padding-panel);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-card);
    background: var(--surface);
    font-size: var(--type-control-size);
  }

  .triggers {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .triggers li {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
    min-height: 28px;
    padding: 0 calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--raised);
    white-space: nowrap;
  }

  .triggers li .code {
    font-size: 12.5px;
  }

  /* On a phone a trigger wraps within its chip, a long webhook path breaking where it must. */
  @media (max-width: 759px) {
    .triggers li {
      flex-wrap: wrap;
      max-width: 100%;
      padding: calc(var(--unit) * 2) calc(var(--unit) * 5);
      white-space: normal;
    }

    .triggers li .code {
      overflow-wrap: anywhere;
    }
  }

  .history {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  .history th,
  .history td {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
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

  .runform {
    max-width: 760px;
    margin-top: calc(var(--unit) * 8);
  }

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 1.55fr) minmax(0, 1fr);
    gap: calc(var(--unit) * 7);
    margin-top: calc(var(--unit) * 8);
  }

  .run {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
    height: 24px;
    margin-bottom: calc(var(--unit) * 5);
    font-size: var(--type-control-size);
    white-space: nowrap;
  }

  .run .info {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .run a {
    color: var(--accent);
  }

  .tabs {
    display: flex;
    gap: calc(var(--unit) * 2);
    margin-bottom: calc(var(--unit) * 6);
  }


  .clone {
    margin: calc(var(--unit) * 5) 0 0;
    padding: calc(var(--unit) * 4) calc(var(--unit) * 6);
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-size: var(--type-identifier-size-min);
  }

  /* Under 1100px, where the sidebar folds, the two columns go one above the other. */
  @media (max-width: 1099px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  /* From 1100px the graph and the step take the height the window has left, however large, and what
     either holds beyond it scrolls inside its pane; a window too short for 560px of them scrolls. */
  @media (min-width: 1100px) {
    .columns {
      flex: 1 0 auto;
      min-height: 560px;
    }

    .columns > :global(.pane) > :global(.body) {
      flex: 1 1 0px;
    }

    .columns :global(.canvas) {
      flex: 1 1 0px;
    }

    .columns :global(.canvas > .scroll) {
      flex: 1 1 0px;
      max-height: none;
    }


    /* The screen is then a column, whose margins add rather than fold into one another: the room
       under what starts it is its own margin alone, as it is where the screen is not a column. */
    .about + .columns,
    .about + .runform {
      margin-top: 0;
    }
  }
</style>
