<script lang="ts">
  import { untrack } from "svelte";
  import type { API, Me } from "../api/client";
  import { commitFile } from "../lib/git/commit";
  import { withPushToken } from "../lib/git/token";
  import { addStep, connect, disconnect, edgesWritten, removeStep, setFanOut, setMaxParallel, setMerge, type Merge } from "../lib/editor/edits";
  import { includedPaths, resolve } from "../lib/editor/resolve";
  import { layout, type Graph } from "../lib/graph";
  import { explain, refused, type Explained } from "../lib/problem";
  import { lineDiff } from "../lib/tree";
  import { check, type Problem as FileProblem } from "../lib/workflow-check";
  import { Refused, YamlTree } from "../lib/yaml-tree";
  import Dialog from "./Dialog.svelte";
  import GraphCanvas from "./GraphCanvas.svelte";
  import Icon from "./Icon.svelte";
  import Notice from "./Notice.svelte";
  import Pane from "./Pane.svelte";
  import Problem from "./Problem.svelte";
  import YamlEditor from "./YamlEditor.svelte";

  // The visual editor: the graph and agentiik.yaml beside it as one model, the file. Each edit of the
  // graph is a splice of the file, shown in the text at once, and the text is edited by hand as
  // well, the graph following it each time it reads. The graph is drawn from the file as it is
  // edited, resolved here as the engine resolves it for a run, its includes read at the version
  // edited; what the browser cannot read is drawn as that version resolved it. Nothing is written
  // back that resolution found: what an included file or a hidden block gives a step stays there.
  // The text is a YAML editor of its own, beside the graph or alone across the page, and what the
  // edits come to is committed from here, over git, as a clone would push it.
  let {
    api,
    me,
    namespace,
    workflow,
    commit,
    entry,
    base,
    cloneURL,
    branch,
    ontoDefault = true,
    layout: shown = "graph",
    selected = $bindable(),
    onlayout,
    onclose,
    oncommitted,
  }: {
    api: API;
    me: Me;
    namespace: string;
    workflow: string;
    commit: string;
    entry: string;
    base: Graph;
    cloneURL: string;
    branch: string;
    // whether the caller moves the default branch, which a protected one takes grant:manage for:
    // where they do not, a commit goes onto a new branch, as the repository would refuse it there.
    ontoDefault?: boolean;
    layout?: "graph" | "yaml";
    selected: string | undefined;
    onlayout: (layout: "graph" | "yaml") => void;
    onclose: () => void;
    oncommitted: (branch: string, commit: string) => void;
  } = $props();

  // The file as it is now, which the YAML editor holds with the history Undo walks back.
  let text = $state(untrack(() => entry));
  const tree = $derived(new YamlTree(text));
  let undoable = $state(0);
  // refusedAt is where the push's refusal put a problem of the file, marked on its line until the
  // text changes.
  let refusedAt = $state<{ line: number; column?: number; message: string }[]>([]);
  let yamlEditor = $state<ReturnType<typeof YamlEditor> | undefined>();
  // good is the last file that read whole, which the graph is drawn from while the text does not.
  let good = $state(untrack(() => new YamlTree(entry)));
  $effect(() => {
    const t = tree;
    if (t.problems.length === 0) untrack(() => (good = t));
  });
  let problem = $state<Explained | null>(null);
  let said = $state("");

  // The files the entry point includes, read at the version edited, before anything is resolved.
  let files = $state<Map<string, string>>(new Map());
  let reading = $state(true);
  $effect(() => {
    untrack(async () => {
      const read = new Map<string, string>();
      const fetchOne = async (path: string) => {
        const { data } = await api.GET("/api/v1/{ns}/workflows/{name}/tree/{ref}", { params: { path: { ns: namespace, name: workflow, ref: commit }, query: { path } }, parseAs: "text" });
        if (typeof data === "string") read.set(path, data);
      };
      // Each round reads what the files read so far include, until nothing new is named.
      let asked = new Set<string>();
      for (;;) {
        const wanted = includedPaths(entry, (p) => read.get(p)).filter((p) => !asked.has(p));
        if (wanted.length === 0) break;
        asked = new Set([...asked, ...wanted]);
        await Promise.all(wanted.map(fetchOne));
      }
      files = read;
      reading = false;
    });
  });

  const resolved = $derived(resolve(good.text, (p) => files.get(p), base));
  const graph = $derived(resolved.graph);
  const laid = $derived(layout(graph));
  const problems = $derived<FileProblem[]>(check(tree));
  const marked = $derived([...problems.map((p) => ({ line: p.line, message: p.message })), ...refusedAt]);
  // What the edits come to, as a review counts it: the lines added and the lines removed, a line
  // changed being one of each.
  const diff = $derived(lineDiff(entry, tree.text));
  const added = $derived(diff.filter((l) => l.kind === "added").length);
  const removed = $derived(diff.filter((l) => l.kind === "removed").length);
  const changes = $derived(added + removed);
  const lines = (n: number) => `${n} ${n === 1 ? "line" : "lines"}`;
  const counted = $derived(changes === 0 ? "No change" : added && removed ? `${lines(added)} added, ${removed} removed` : added ? `${lines(added)} added` : `${lines(removed)} removed`);
  const step = $derived(selected && graph.steps[selected] ? selected : undefined);
  const here = $derived(step !== undefined && resolved.written.get(step) === "entry");

  // edit makes one edit, which the YAML editor makes in its text and keeps in its history, or says
  // why it was refused.
  function edit(change: (t: YamlTree) => YamlTree, done?: string) {
    try {
      const next = change(tree);
      if (next.text === tree.text) return;
      text = next.text;
      problem = null;
      if (done) said = done;
    } catch (e) {
      problem = refused("make this change", e instanceof Refused ? sentenceOf(e.message) : String(e));
    }
  }

  // A refusal is said as written, ending with a full stop: one that begins with a step, a port or a
  // key begins with it as the file spells it, never capitalised.
  const sentenceOf = (s: string) => s + (s.endsWith(".") ? "" : ".");

  function undo() {
    yamlEditor?.undo();
  }

  function undoAll() {
    if (text === entry && undoable === 0) return;
    text = entry;
    yamlEditor?.reset(entry);
  }

  function typed(next: string, history: { undo: number }) {
    text = next;
    undoable = history.undo;
    refusedAt = [];
  }

  // A step added, in a dialog.
  let adding = $state(false);
  let name = $state("");
  let image = $state("");
  let ports = $state("out");
  function add(e: SubmitEvent) {
    e.preventDefault();
    const outputs = ports.split(/[\s,]+/).filter(Boolean);
    const before = tree.text;
    edit((t) => addStep(t, graph, name.trim(), image, outputs), `${name.trim()} is added.`);
    if (tree.text !== before) {
      selected = name.trim();
      adding = false;
      name = image = "";
      ports = "out";
    }
  }

  // An edge connected to the step chosen: a port of another step, onto an input port of this one.
  let from = $state("");
  let as = $state("in");
  const sources = $derived(graph.order.filter((s) => s !== step).flatMap((s) => (graph.steps[s]?.outputs ?? ["out"]).map((p) => `${s}.${p}`)));
  function wire(e: SubmitEvent) {
    e.preventDefault();
    if (!step || !from) return;
    const [s, p] = [from.slice(0, from.lastIndexOf(".")), from.slice(from.lastIndexOf(".") + 1)];
    edit((t) => connect(t, graph, { step: s, port: p }, { step: step!, as: as.trim() }));
  }

  const edges = $derived(step && here ? edgesWritten(tree, step) : []);
  const values = $derived(step ? (graph.steps[step] as unknown as { merge?: unknown; strategy?: { fan_out?: string; max_parallel?: number } }) : undefined);
  const merge = $derived(typeof values?.merge === "string" ? values.merge : values?.merge ? "join" : "wait_all");
  const fanOut = $derived(values?.strategy?.fan_out ?? "none");
  let batch = $state(10);

  function download() {
    const url = URL.createObjectURL(new Blob([tree.text], { type: "application/yaml" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "agentiik.yaml";
    a.click();
    URL.revokeObjectURL(url);
  }

  // The commit: a message and the branch it lands on, the default branch the file was opened at or
  // a new one starting there, sent over git as the person signed in, with a token minted for the
  // push alone (lib/git/token), since the repository takes no session.
  let committing = $state(false);
  let message = $state("Update agentiik.yaml");
  let target = $state<"default" | "new">(untrack(() => (ontoDefault ? "default" : "new")));
  let newBranch = $state("");
  let pushing = $state(false);
  let commitProblem = $state<Explained | null>(null);

  async function send(e: SubmitEvent) {
    e.preventDefault();
    if (pushing) return;
    const onto = target === "new" ? newBranch.trim() : branch;
    pushing = true;
    commitProblem = null;
    try {
      const login = me.user?.login ?? me.principal;
      const id = await withPushToken(api, cloneURL, (remote) =>
        commitFile({
          remote,
          parent: commit,
          branch: onto,
          create: target === "new",
          path: "agentiik.yaml",
          text,
          message: message.trim(),
          author: { name: me.user?.display_name ?? login, email: me.user?.email || `${login}@${new URL(cloneURL).hostname}`, when: new Date() },
        }),
      );
      committing = false;
      oncommitted(onto, id);
    } catch (err) {
      commitProblem = explain(`commit to ${onto}`, err);
      const at = /agentiik\.yaml:(\d+)(?::(\d+))?/.exec(err instanceof Error ? err.message : "");
      if (at) {
        refusedAt = [{ line: Number(at[1]), column: at[2] ? Number(at[2]) : undefined, message: err instanceof Error ? err.message : String(err) }];
      }
    } finally {
      pushing = false;
    }
  }

  // Leaving with changes asks first, since nothing keeps them.
  $effect(() => {
    if (changes === 0) return;
    const ask = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", ask);
    return () => window.removeEventListener("beforeunload", ask);
  });
</script>

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<div class="bar" role="toolbar" aria-label="The editor">
  <span class="count" class:none={changes === 0}>{counted}</span>
  <span class="choice" role="group" aria-label="Shown">
    <button class="control" aria-pressed={shown === "graph"} onclick={() => onlayout("graph")}><Icon name="control-workflows" size={14} />Graph</button>
    <button class="control" aria-pressed={shown === "yaml"} onclick={() => onlayout("yaml")}><Icon name="control-format" size={14} />YAML</button>
  </span>
  {#if shown === "graph"}<button class="control" onclick={() => (adding = true)}><Icon name="control-add" size={14} />Add a step</button>{/if}
  <button class="control" disabled={undoable === 0} onclick={undo}>Undo</button>
  <button class="control" disabled={changes === 0} onclick={undoAll}>Undo every change</button>
  <span class="right">
    <button class="control" disabled={changes === 0} onclick={download}><Icon name="control-download" size={14} />Download agentiik.yaml</button>
    <button class="control primary" disabled={changes === 0} onclick={() => { commitProblem = null; committing = true; }}>Commit</button>
    <button class="control" onclick={onclose}>Stop editing</button>
  </span>
</div>

<div class="columns" class:yaml={shown === "yaml"}>
  {#if shown === "graph"}
    <Pane title="Graph" aside={reading ? "loading" : resolved.problems.length ? `${resolved.problems.length} ${resolved.problems.length === 1 ? "problem" : "problems"}` : ""} focused>
      <GraphCanvas {graph} {laid} run={null} {selected} onselect={(s) => (selected = s)} />
      {#if resolved.problems.length}
        <ul class="said">{#each resolved.problems as p (p)}<li class="muted">{p}</li>{/each}</ul>
      {/if}
    </Pane>
  {/if}

  <div class="side">
    {#if shown === "graph"}
      <Pane title={step ?? "Step"} aside={step && !here ? "included file" : ""}>
        {#if !step}
          <p class="muted">Select a step</p>
        {:else if !here}
          <p class="muted">Defined in an included file.</p>
        {:else}
          <h3 class="sub">Its edges</h3>
          <ul class="edges">
            {#each edges as e, i (i)}
              <li><span class="term">{e.step}.{e.port}</span><span class="faint">onto</span><span class="term">{e.as}</span><button class="control" aria-label="Take the edge from {e.step}.{e.port} away" onclick={() => edit((t) => disconnect(t, step!, i))}>Take away</button></li>
            {:else}
              <li class="muted">None</li>
            {/each}
          </ul>
          <form class="row" onsubmit={wire} aria-label="Connect a port to {step}">
            <select bind:value={from} aria-label="The port it takes">
              <option value="" disabled>a port of another step</option>
              {#each sources as s (s)}<option value={s}>{s}</option>{/each}
            </select>
            <input bind:value={as} aria-label="The input port it arrives on" class="term narrow" />
            <button class="control" disabled={!from}>Connect</button>
          </form>

          <h3 class="sub">How it runs</h3>
          <label class="row">
            <span class="muted label">merge</span>
            <select value={merge} aria-label="merge" disabled={merge === "join"} onchange={(e) => edit((t) => setMerge(t, step!, (e.currentTarget as HTMLSelectElement).value as Merge))}>
              {#each ["wait_all", "zip", "first"] as m (m)}<option value={m}>{m}</option>{/each}
              {#if merge === "join"}<option value="join">join</option>{/if}
            </select>
          </label>
          <div class="row">
            <span class="muted label">fan_out</span>
            <select value={fanOut.startsWith("batch(") ? "batch" : fanOut} aria-label="fan_out" onchange={(e) => {
              const v = (e.currentTarget as HTMLSelectElement).value;
              edit((t) => setFanOut(t, step!, v === "batch" ? `batch(${batch})` : v));
            }}>
              {#each ["none", "item", "batch"] as f (f)}<option value={f}>{f === "batch" ? "batch(n)" : f}</option>{/each}
            </select>
            {#if fanOut.startsWith("batch(")}
              <input type="number" min="1" class="narrow" aria-label="Items a batch" value={Number(fanOut.slice(6, -1))} onchange={(e) => {
                batch = Number((e.currentTarget as HTMLInputElement).value);
                edit((t) => setFanOut(t, step!, `batch(${batch})`));
              }} />
            {/if}
          </div>
          <label class="row">
            <span class="muted label">max_parallel</span>
            <input type="number" min="1" class="narrow" aria-label="max_parallel" placeholder="no bound" value={values?.strategy?.max_parallel ?? ""} onchange={(e) => {
              const raw = (e.currentTarget as HTMLInputElement).value;
              edit((t) => setMaxParallel(t, step!, raw === "" ? null : Number(raw)));
            }} />
          </label>
          <p class="row"><button class="control danger" onclick={() => edit((t) => removeStep(t, graph, step!), `${step} is removed.`)}><Icon name="control-remove" size={14} />Remove the step</button></p>
        {/if}
      </Pane>
    {/if}

    <Pane title="agentiik.yaml" aside={marked.length ? `${marked.length} ${marked.length === 1 ? "problem" : "problems"}` : "valid"}>
      <YamlEditor bind:this={yamlEditor} value={text} label="agentiik.yaml, as it is edited" problems={marked} onchange={typed} />
      {#if marked.length}
        <ul class="problems">
          {#each marked as p, i (i)}
            <li><button class="line term" onclick={() => yamlEditor?.reveal(p.line)}>line {p.line}</button> {p.message}</li>
          {/each}
        </ul>
      {/if}
    </Pane>
  </div>
</div>

<Dialog title="Commit" bind:open={committing}>
  {#if commitProblem}<Problem explained={commitProblem} />{/if}
  <form class="form" onsubmit={send} aria-label="Commit">
    <label><span>Message</span><textarea bind:value={message} class="message" rows="3" required></textarea></label>
    {#if ontoDefault}
      <fieldset>
        <legend>Branch</legend>
        <label class="check"><input type="radio" name="target" value="default" bind:group={target} /><span class="term">{branch}</span></label>
        <label class="check"><input type="radio" name="target" value="new" bind:group={target} />A new branch</label>
        {#if target === "new"}
          <input bind:value={newBranch} class="term" aria-label="The new branch" placeholder="try-retries" required pattern={"[A-Za-z0-9._/-]+"} />
        {/if}
      </fieldset>
    {:else}
      <label><span>The new branch</span><input bind:value={newBranch} class="term" placeholder="try-retries" required pattern={"[A-Za-z0-9._/-]+"} /></label>
    {/if}
    <p class="buttons"><button class="control primary" disabled={pushing}>Commit</button></p>
  </form>
</Dialog>

<Dialog title="Add a step" bind:open={adding}>
  <form class="form" onsubmit={add} aria-label="Add a step">
    <label><span>Name</span><input bind:value={name} class="term" required placeholder="notify" /></label>
    <label><span>Image</span><input bind:value={image} class="term" required placeholder="ghcr.io/acme/agk-notify:1.0.0" /></label>
    <label><span>Output ports</span><input bind:value={ports} class="term" placeholder="ok, rejected" required /></label>
    <p class="buttons"><button class="control primary">Add the step</button><button class="control" type="button" onclick={() => (adding = false)}>Cancel</button></p>
  </form>
</Dialog>

<style>
  .bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 3);
    margin-bottom: calc(var(--unit) * 3);
  }

  .bar .right {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3);
    margin-left: auto;
  }

  .count {
    min-width: 9em;
    color: var(--accent);
    font-weight: 600;
  }

  .count.none {
    color: var(--muted);
    font-weight: 400;
  }

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 1.4fr) minmax(320px, 1fr);
    gap: calc(var(--unit) * 6);
    align-items: start;
  }

  .side {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  .sub {
    margin: calc(var(--unit) * 4) 0 calc(var(--unit) * 2);
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 600;
  }

  .sub:first-child {
    margin-top: 0;
  }

  .edges {
    display: grid;
    gap: calc(var(--unit) * 2);
    margin: 0 0 calc(var(--unit) * 3);
    padding: 0;
    list-style: none;
  }

  .edges li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .edges button {
    margin-left: auto;
  }

  .row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 3);
    margin: 0 0 calc(var(--unit) * 3);
  }

  .label {
    min-width: 7em;
  }

  .narrow {
    width: 7em;
  }

  .problems,
  .said {
    margin: calc(var(--unit) * 3) 0 0;
    padding: 0;
    list-style: none;
    font-size: var(--type-control-size);
  }

  .problems li {
    display: flex;
    align-items: baseline;
    gap: calc(var(--unit) * 3);
    color: var(--failed);
  }

  /* A problem's line, which puts the cursor on it. */
  .line {
    flex: none;
    padding: 0;
    border: none;
    background: none;
    color: var(--faint);
    font-size: var(--type-control-size);
    cursor: pointer;
  }

  .line:hover {
    color: var(--text);
    text-decoration: underline;
  }

  /* Graph or YAML: two buttons drawn as one control, the one shown pressed. */
  .choice {
    display: inline-flex;
  }

  .choice .control + .control {
    margin-left: calc(-1 * var(--border-hairline));
  }

  .choice .control:first-child {
    border-top-right-radius: 0;
    border-bottom-right-radius: 0;
  }

  .choice .control:last-child {
    border-top-left-radius: 0;
    border-bottom-left-radius: 0;
  }

  .choice .control[aria-pressed="true"] {
    position: relative;
    border-color: var(--accentLine);
    background: var(--accentDim);
    color: var(--accent);
  }

  /* The YAML alone, across the page and as tall as the window, since a file is read down. */
  .columns.yaml {
    grid-template-columns: minmax(0, 1fr);
  }

  .columns.yaml .side {
    --editor-height: calc(100vh - 220px);
  }

  .form fieldset {
    display: grid;
    gap: calc(var(--unit) * 3);
    margin: 0;
    padding: 0;
    border: none;
  }

  .form legend {
    margin-bottom: calc(var(--unit) * 2);
    padding: 0;
    color: var(--muted);
  }

  .form label > span:first-child {
    color: var(--muted);
  }

  .form label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .form label.check input {
    margin: 0;
  }

  .message {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 5);
    resize: vertical;
    --leading: 1.5;
  }

  .form {
    display: grid;
    gap: calc(var(--unit) * 5);
  }

  .form label {
    display: grid;
    gap: calc(var(--unit) * 2);
  }

  .buttons {
    display: flex;
    gap: calc(var(--unit) * 3);
    margin: 0;
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  @media (max-width: 1099px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  /* On a phone the count takes a line of its own, and the buttons follow it from the left. */
  @media (max-width: 759px) {
    .count {
      flex-basis: 100%;
    }

    .bar .right {
      margin-left: 0;
    }
  }
</style>
