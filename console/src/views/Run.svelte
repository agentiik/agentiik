<script lang="ts">
  import { untrack } from "svelte";
  import type { API, Me } from "../api/client";
  import CompareWith from "../components/CompareWith.svelte";
  import EnvelopePane from "../components/EnvelopePane.svelte";
  import Icon from "../components/Icon.svelte";
  import LogPane from "../components/LogPane.svelte";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import Refused from "./Refused.svelte";
  import { cancelRun, replayRun } from "../lib/actions";
  import { tokens } from "../lib/envelope";
  import { address, fetchable, retention } from "../lib/artifacts";
  import { band } from "../lib/exit";
  import { between, clock, took } from "../lib/format";
  import { holds } from "../lib/permissions";
  import { follow, type Place } from "../lib/place.svelte";
  import { lastAttempt, RunReader, tasksOf, type EnvelopeReference, type TaskSummary } from "../lib/run.svelte";
  import { sentence } from "../lib/signin";

  // The run inspector: one run, each step's verdict and shards on the left, and on the right the step
  // chosen, its tasks, the exit code of the one chosen and what it means, the envelopes its ports
  // published and were handed, by digest, size and item count, and the log of the task chosen. What
  // an envelope holds is envelope contents, which run:read_data guards, and a principal without it
  // is shown no pane of it at all. Cancelling the run and replaying it are workflow:run's, and a
  // principal without it is shown neither.
  let { api, place, me, namespace, id }: { api: API; place: Place; me: Me; namespace: string; id: string } = $props();

  const reader = $derived(new RunReader(api, id));
  let now = $state(Date.now());

  $effect(() => {
    const r = reader;
    untrack(() => r.read());
  });

  // Read again every five seconds until the run has ended, and move the durations every second.
  $effect(() => {
    const r = reader;
    const reading = setInterval(() => {
      if (!r.ended && document.visibilityState === "visible") {
        r.read();
      }
    }, 5000);
    const ticking = setInterval(() => (now = Date.now()), 1000);
    return () => {
      clearInterval(reading);
      clearInterval(ticking);
    };
  });

  const run = $derived(reader.run);
  // A run read under another namespace's address is not this address's run: the address names both.
  const here = $derived(run !== null && run.namespace === namespace);

  // The step chosen: the one the address names, or the first that failed, or the last that ran.
  const chosenStep = $derived.by(() => {
    if (!run) return undefined;
    const named = place.query.get("step");
    if (named && run.steps.some((s) => s.step === named)) return named;
    return (run.steps.find((s) => s.verdict === "failed") ?? [...run.steps].reverse().find((s) => s.verdict !== "pending") ?? run.steps[0])?.step;
  });
  const step = $derived(run?.steps.find((s) => s.step === chosenStep));
  const tasks = $derived(run && chosenStep ? tasksOf(run, chosenStep) : []);

  // The task chosen: the one the address names, or the first of the last attempt that failed.
  const task = $derived.by((): TaskSummary | undefined => {
    const named = place.query.get("task");
    return tasks.find((t) => t.task === named) ?? tasks.find((t) => t.state === "failed" || t.state === "timed_out" || t.state === "lost") ?? tasks[0];
  });

  const readsData = $derived(run ? holds(me, "run:read_data", run.namespace, run.workflow) : false);
  const mayRun = $derived(run ? holds(me, "workflow:run", run.namespace, run.workflow) : false);

  // The step pane's tab and the port chosen in it, which the address keeps beside the step and the
  // task: what one engineer sends another is the screen they are looking at.
  type Tab = "output" | "input" | "files" | "logs";
  const tab = $derived.by((): Tab => {
    const named = place.query.get("pane");
    return named === "input" || named === "files" || named === "logs" ? named : "output";
  });
  const ports = $derived<Record<string, EnvelopeReference>>((tab === "input" ? task?.inputs : step?.ports) ?? {});
  // Every port the step declares on the side shown, those nothing was handed or published on yet
  // among them, then any the run holds an envelope for that the version did not name.
  const portNames = $derived([...new Set([...((tab === "input" ? step?.input_ports : step?.output_ports) ?? []), ...Object.keys(ports)])]);
  const port = $derived.by(() => {
    const named = place.query.get("port");
    return named && named in ports ? named : Object.keys(ports)[0];
  });

  // The files the step chosen published, and the workflow output whose envelope is open, which the
  // address keeps too.
  const files = $derived(run && chosenStep ? run.artifacts.filter((a) => a.step === chosenStep) : []);
  const output = $derived.by(() => {
    const named = place.query.get("output");
    return readsData && named && run?.outputs && named in run.outputs ? named : undefined;
  });

  // Fetching a file asks the route first, which spends nothing, so that one gone since the run was
  // read is said to be finished rather than opening the API's refusal in place of the console.
  let unfetched = $state("");

  async function fetchFile(uri: string, name: string) {
    unfetched = "";
    const url = new URL(address(uri), document.baseURI).href;
    const why = await fetchable(globalThis.fetch.bind(globalThis), url);
    if (why) {
      unfetched = why;
      return;
    }
    const link = document.createElement("a");
    link.href = url;
    link.download = name;
    link.click();
  }

  // What was last asked of the run, and why it was refused where it was. Cancelling asks once more
  // before it is sent, since it stops the tasks in flight.
  let confirming = $state(false);
  let acting = $state(false);
  let said = $state("");
  let problem = $state("");

  async function act(work: () => Promise<void>) {
    if (acting) return;
    acting = true;
    problem = "";
    said = "";
    try {
      await work();
    } catch (e) {
      problem = sentence(e instanceof Error ? e.message : String(e));
    } finally {
      acting = false;
      confirming = false;
    }
  }

  function cancel() {
    return act(async () => {
      if (!run) return;
      await cancelRun(api, run.run);
      said = "Cancelling was asked: the controller stops the tasks in flight, and the run ends cancelled.";
      await reader.read();
    });
  }

  function replay(from?: string) {
    return act(async () => {
      if (!run) return;
      const started = await replayRun(api, run.run, from);
      place.go({ kind: "namespace", namespace: run.namespace, view: "runs", run: started });
    });
  }

  function choose(query: Record<string, string>) {
    const q = new URLSearchParams(place.query);
    for (const [k, v] of Object.entries(query)) q.set(k, v);
    if (query.step) {
      q.delete("task");
      q.delete("port");
    }
    place.narrow(q);
  }

  function shortDigest(d: string): string {
    return d.replace(/^sha256:/, "").slice(0, 12);
  }

  // shortImage is an image by its repository and the first characters of its digest, which is
  // what a person compares at a glance; the whole reference is the element's title.
  function shortImage(ref: string): string {
    const [repository, digest] = ref.split("@sha256:");
    return digest ? `${repository}@${digest.slice(0, 12)}` : ref;
  }

  function bytes(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
    return `${(n / 1024 / 1024).toFixed(1)} MiB`;
  }

  function lasted(start?: string, end?: string): string {
    const ms = between(start, end, now);
    return ms === undefined ? "" : took(ms);
  }

  const runs = $derived({ kind: "namespace" as const, namespace, view: "runs" as const });
</script>

{#if reader.missing || (run && !here)}
  <Refused />
{:else if reader.refused}
  <Pane title="Run"><p class="refused" role="alert">The run could not be read: {reader.refused}</p></Pane>
{:else if run}
  <div class="inspector">
    <Pane title="Run" aside="{run.namespace}/{run.workflow}@{run.commit.slice(0, 7)}">
      <div class="head">
        <StatePill state={run.state} live={!reader.ended} />
        <span class="mono id">{run.run}</span>
        <span class="mono name">{run.workflow}</span>
        <span class="muted">
          <span class="mono">{run.trigger_kind}</span>
          · created <time class="mono" datetime={run.created_at} title={run.created_at}>{clock(run.created_at, now)}</time>
          {#if run.started_at}· took <span class="mono">{lasted(run.started_at, run.finished_at)}</span>{/if}
          · by <span class="mono">{run.triggered_by}</span>
        </span>
        <a class="back" href={place.href(runs)} onclick={follow(place, runs)}>All runs of {namespace}</a>
      </div>
      {#if mayRun || reader.ended}
        <div class="actions">
          {#if mayRun && !reader.ended}
            {#if confirming}
              <span>Cancel this run? Its tasks in flight are stopped.</span>
              <button class="control danger" disabled={acting} onclick={cancel}><Icon name="control-cancel" size={14} />Cancel run</button>
              <button class="control" disabled={acting} onclick={() => (confirming = false)}>Keep it running</button>
            {:else}
              <button class="control" disabled={acting} onclick={() => (confirming = true)}><Icon name="control-cancel" size={14} />Cancel run</button>
            {/if}
          {:else if mayRun}
            {#if chosenStep && !run.replay_from_start_only}
              <button class="control primary" disabled={acting} onclick={() => replay(chosenStep)}><Icon name="control-replay" size={14} />Replay from {chosenStep}</button>
            {/if}
            <button class="control" disabled={acting} onclick={() => replay()}><Icon name="control-replay" size={14} />Replay from the start</button>
            {#if run.replay_from_start_only}
              <span class="muted">An input a step would restart from has been purged, so this run replays from its start only.</span>
            {/if}
          {/if}
          <!-- Reading a run against another needs nothing but run:read, which reading this one took. -->
          {#if reader.ended}<CompareWith {api} {place} {run} />{/if}
        </div>
        {#if said}<p class="muted" role="status">{said}</p>{/if}
        {#if problem}<p class="refused" role="alert">{problem}</p>{/if}
      {/if}
      {#if run.reason}<p class="reason">{run.reason}</p>{/if}
      {#if run.replay_of}
        <p class="muted">Replays <span class="mono">{run.replay_of}</span>{#if run.replay_from}&nbsp;from <span class="mono">{run.replay_from}</span>{/if}.</p>
      {/if}
      <ol class="path" aria-label="Steps in order">
        {#each run.steps as s (s.step)}
          <li class={s.verdict}><span class="dot" aria-hidden="true"></span><span class="mono">{s.step}</span></li>
        {/each}
      </ol>
    </Pane>

    <div class="columns">
      <Pane title="Steps" aside={String(run.steps.length)}>
        <ul class="steps">
          {#each run.steps as s (s.step)}
            {@const cells = lastAttempt(run, s.step)}
            <li>
              <button class="step" class:chosen={s.step === chosenStep} aria-pressed={s.step === chosenStep} onclick={() => choose({ step: s.step })}>
                <StatePill state={s.verdict} live={!reader.ended} />
                <span class="mono name">{s.step}</span>
                <span class="mono muted took">{lasted(s.started_at, s.finished_at)}</span>
                <span class="sub muted">
                  {#if s.verdict === "pending" && s.attempts === 0}
                    not reached
                  {:else}
                    {cells.length > 1 || cells[0]?.shard ? `${cells.length} shards · ` : ""}attempt {s.attempts}
                  {/if}
                </span>
                {#if cells.length > 1 || cells[0]?.shard}
                  <span class="cells" aria-hidden="true">
                    {#each cells as c (c.task)}<span class="cell {c.state}"></span>{/each}
                  </span>
                {/if}
              </button>
            </li>
          {/each}
        </ul>
        {#if run.outputs && Object.keys(run.outputs).length > 0}
          <h3>Workflow outputs</h3>
          <table class="outputs">
            <thead><tr><th>Output</th><th>From</th><th class="number">Items</th><th>Files</th></tr></thead>
            <tbody>
              {#each Object.entries(run.outputs) as [name, out] (name)}
                {@const held = run.artifacts.filter((a) => a.step === out.step && a.port === out.port)}
                <tr class:chosen={name === output}>
                  <td class="mono">
                    {#if readsData}
                      <button class="link" aria-pressed={name === output} onclick={() => choose({ output: name })}>{name}</button>
                    {:else}
                      {name}
                    {/if}
                  </td>
                  <td class="mono muted">{out.step}.{out.port}</td>
                  <td class="number mono">{out.count}</td>
                  <td class="muted">{#if held.length === 0}none{:else}{held.length} · {retention(held[0]!, now)}{/if}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        {/if}
      </Pane>

      {#if step}
        <Pane title={task?.shard ? `${step.step} · shard ${task.shard.index}/${task.shard.of} · attempt ${task.attempt}` : task ? `${step.step} · attempt ${task.attempt}` : step.step} focused>
          {#if task?.exit_code !== undefined}
            {@const meaning = band(task.exit_code)}
            <p class="exit {meaning.tone}">
              <strong class="mono">exit {task.exit_code}</strong>
              <span>{meaning.name}: {meaning.handling}</span>
            </p>
          {/if}

          <dl class="header">
            {#if step.image}
              <dt>Image</dt>
              <dd class="mono" title={step.image}>{shortImage(step.image)}</dd>
            {/if}
            {#if task}
              <dt>Task</dt>
              <dd class="mono">attempt {task.attempt}{#if task.shard}&nbsp;· shard {task.shard.index} of {task.shard.of}{/if}</dd>
              <dt>Runner</dt>
              <dd class="mono">{task.runner ?? (task.memoised_from ? `none, a cache hit of ${task.memoised_from}` : task.called ? `none, it called ${task.called}` : "not held yet")}</dd>
            {/if}
          </dl>
          {#if readsData && task?.params && Object.keys(task.params).length > 0}
            <details class="params">
              <summary>Parameters it was dispatched with</summary>
              <pre class="json"><code>{#each tokens(task.params) as t, i (i)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
            </details>
          {:else if task && !readsData && task.state !== "pending"}
            <p class="faint">The parameters it was dispatched with are not shown: you do not hold run:read_data on {run.namespace}/{run.workflow}.</p>
          {/if}

          <table class="tasks">
            <thead>
              <tr><th>Task</th><th>State</th><th>Runner</th><th class="number">Exit</th><th class="number">Took</th></tr>
            </thead>
            <tbody>
              {#each tasks as t (t.task)}
                <tr class:chosen={t.task === task?.task} onclick={() => choose({ step: step.step, task: t.task })}>
                  <td class="mono">
                    <button class="link" onclick={(e) => { e.stopPropagation(); choose({ step: step.step, task: t.task }); }}>
                      {t.shard ? `${t.shard.index}/${t.shard.of} · ` : ""}attempt {t.attempt}
                    </button>
                  </td>
                  <td><StatePill state={t.state} live={!reader.ended} /></td>
                  <td class="mono muted">{t.runner ?? (t.memoised_from ? `cache hit of ${t.memoised_from}` : t.called ? `called ${t.called}` : "")}</td>
                  <td class="number mono">{t.exit_code ?? ""}</td>
                  <td class="number mono">{lasted(t.started_at, t.finished_at)}</td>
                </tr>
              {:else}
                <tr><td colspan="5" class="muted empty">No task of this step has been created.</td></tr>
              {/each}
            </tbody>
          </table>

          <div class="ports">
            <div class="sides" role="tablist" aria-label="What the step pane shows">
              <button role="tab" aria-selected={tab === "output"} onclick={() => choose({ pane: "output" })}>Output</button>
              <button role="tab" aria-selected={tab === "input"} onclick={() => choose({ pane: "input" })}>Input</button>
              <button role="tab" aria-selected={tab === "files"} onclick={() => choose({ pane: "files" })}>Files</button>
              <button role="tab" aria-selected={tab === "logs"} onclick={() => choose({ pane: "logs" })}>Logs</button>
            </div>
            {#if tab === "files"}
              {#if files.length === 0}
                <p class="muted">The step has published no file.</p>
              {:else}
                <table class="files">
                  <thead><tr><th>File</th><th>Port</th><th>Media type</th><th class="number">Size</th><th>SHA-256</th><th>Retention</th><th></th></tr></thead>
                  <tbody>
                    {#each files as f (f.uri)}
                      <tr class={f.status}>
                        <td class="mono">{f.name}</td>
                        <td class="mono muted port {f.port}">{f.port}</td>
                        <td class="mono muted">{f.media_type}</td>
                        <td class="number mono">{bytes(f.size)}</td>
                        <td class="mono muted" title={f.sha256}>{f.sha256.slice(0, 12)}</td>
                        <td class="muted">{retention(f, now)}</td>
                        <td class="end">
                          {#if readsData && f.status === "live"}
                            <button class="control" onclick={() => fetchFile(f.uri, f.name)}><Icon name="control-download" size={14} />Download</button>
                          {/if}
                        </td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
                {#if unfetched}<p class="refused" role="alert">{unfetched}</p>{/if}
                {#if !readsData}
                  <p class="faint">The files are listed and not fetched: you do not hold run:read_data on {run.namespace}/{run.workflow}.</p>
                {/if}
              {/if}
            {:else if tab === "logs"}
              {#if task}
                <LogPane {api} run={run.run} step={step.step} task={task.task} />
              {:else}
                <p class="muted">No task of this step has been created, so there is no log yet.</p>
              {/if}
            {:else if portNames.length === 0}
              <p class="muted">{tab === "output" ? "The step declares no output port." : "The step declares no input port."}</p>
            {:else}
              <table class="envelopes">
                <thead><tr><th>Port</th><th class="number">Items</th><th class="number">Size</th><th>Digest</th></tr></thead>
                <tbody>
                  {#each portNames as name (name)}
                    {@const e = ports[name]}
                    <tr class:chosen={readsData && e && name === port}>
                      <td class="mono name port {name}">
                        {#if readsData && e}
                          <button class="link" aria-pressed={name === port} onclick={() => choose({ pane: tab, port: name })}>{name}</button>
                        {:else}
                          {name}
                        {/if}
                      </td>
                      {#if e}
                        <td class="number mono">{e.items}</td>
                        <td class="number mono">{bytes(e.size)}</td>
                        <td class="mono muted" title={e.digest}>{shortDigest(e.digest)}{#if e.purged_at}&nbsp;· purged{/if}</td>
                      {:else}
                        <td colspan="3" class="muted">{tab === "output" ? "published when the step ends" : task ? "not handed to the task chosen" : "handed once a task is dispatched"}</td>
                      {/if}
                    </tr>
                  {/each}
                </tbody>
              </table>
              {#if readsData && port}
                <EnvelopePane {api} run={run.run} step={step.step} {port} side={tab} task={tab === "input" ? task : undefined} />
              {/if}
            {/if}
            {#if !readsData && (tab === "output" || tab === "input")}
              <p class="faint">What the envelopes hold is not shown: you do not hold run:read_data on {run.namespace}/{run.workflow}.</p>
            {/if}
          </div>
        </Pane>
      {/if}
    </div>

    {#if output && run.outputs?.[output]}
      <Pane title="Workflow output {output}" aside="{run.outputs[output].step}.{run.outputs[output].port}">
        <EnvelopePane {api} run={run.run} step={run.outputs[output].step} port={run.outputs[output].port} side="output" {output} />
      </Pane>
    {/if}
  </div>
{/if}

<style>
  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    margin-top: calc(var(--unit) * 6);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  .inspector {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 12);
  }

  .head {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 6);
    font-size: var(--type-identifier-size-max);
  }

  .id {
    font-weight: 600;
  }

  .name {
    font-weight: 600;
  }

  .back {
    margin-left: auto;
    font-size: var(--type-control-size);
  }

  .reason {
    margin: calc(var(--unit) * 5) 0 0;
    color: var(--failed);
  }

  .path {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    margin: calc(var(--unit) * 6) 0 0;
    padding: 0;
    list-style: none;
    color: var(--muted);
    font-size: var(--type-identifier-size-min);
  }

  .path li {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
  }

  .path .dot {
    width: 7px;
    height: 7px;
    border-radius: var(--radius-round);
    background: var(--faint);
  }

  .path .succeeded .dot {
    background: var(--succeeded);
  }

  .path .failed .dot {
    background: var(--failed);
  }

  .path .running .dot {
    background: var(--running);
  }

  .columns {
    display: grid;
    grid-template-columns: 380px 1fr;
    gap: calc(var(--unit) * 7);
    align-items: start;
  }

  .steps {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .step {
    display: grid;
    grid-template-columns: auto 1fr auto;
    align-items: center;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 5);
    width: 100%;
    padding: calc(var(--unit) * 5) calc(var(--unit) * 5);
    border: none;
    border-left: 3px solid transparent;
    border-radius: var(--radius-control);
    background: none;
    text-align: left;
    cursor: pointer;
  }

  .step:hover {
    background: var(--raised);
  }

  .step.chosen {
    border-left-color: var(--accent);
    background: var(--raised);
  }

  .step .took {
    font-size: var(--type-identifier-size-min);
  }

  .step .sub {
    grid-column: 2 / 4;
    font-size: var(--type-identifier-size-min);
  }

  .cells {
    grid-column: 2 / 4;
    display: flex;
    gap: 3px;
  }

  .cell {
    width: 12px;
    height: 8px;
    border-radius: 2px;
    background: var(--line);
  }

  .cell.succeeded {
    background: var(--succeeded);
  }

  .cell.failed,
  .cell.timed_out,
  .cell.lost {
    background: var(--failed);
  }

  .cell.running,
  .cell.dispatched,
  .cell.publishing {
    background: var(--running);
  }

  h3 {
    margin: calc(var(--unit) * 9) 0 calc(var(--unit) * 3);
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .outputs {
    margin-bottom: calc(var(--unit) * 6);
  }

  .envelopes tr.chosen td,
  .outputs tr.chosen td {
    background: var(--raised);
  }

  .files tr.expired td,
  .files tr.collected td {
    color: var(--faint);
  }

  .end {
    text-align: right;
  }

  .header {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 6);
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-identifier-size-min);
  }

  .header dt {
    color: var(--faint);
  }

  .header dd {
    margin: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .params {
    margin: 0 0 calc(var(--unit) * 6);
  }

  .params summary {
    color: var(--accent);
    cursor: pointer;
    font-size: var(--type-control-size);
  }

  .json {
    max-height: 320px;
    margin: calc(var(--unit) * 3) 0 0;
    padding: calc(var(--unit) * 4) calc(var(--unit) * 5);
    overflow: auto;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-family: var(--type-identifier-font);
    font-size: 12px;
    line-height: 1.55;
  }

  .t-key {
    color: var(--accent);
  }

  .t-string {
    color: var(--waiting);
  }

  .t-number,
  .t-literal {
    color: var(--succeeded);
  }

  .exit {
    display: flex;
    align-items: baseline;
    gap: calc(var(--unit) * 5);
    margin: 0 0 calc(var(--unit) * 7);
    padding: calc(var(--unit) * 4) calc(var(--unit) * 6);
    border-radius: var(--radius-innerPanel);
    font-size: var(--type-navigation-size);
  }

  .exit.failed {
    background: var(--failedFill);
    color: var(--failed);
  }

  .exit.waiting {
    background: var(--waitingFill);
    color: var(--waiting);
  }

  .exit.succeeded {
    background: var(--succeededFill);
    color: var(--succeeded);
  }

  .exit.quiet {
    background: var(--sunken);
    color: var(--muted);
  }

  .exit span {
    color: var(--text);
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
    height: 36px;
    padding: 0 calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
    font-size: var(--type-identifier-size-min);
    white-space: nowrap;
  }

  .tasks tbody tr {
    cursor: pointer;
  }

  .tasks tbody tr:hover,
  .tasks tbody tr.chosen {
    background: var(--raised);
  }

  .tasks tr.chosen td:first-child {
    box-shadow: inset 3px 0 0 var(--accent);
  }

  .number {
    text-align: right;
  }

  .link {
    padding: 0;
    border: none;
    background: none;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }

  .empty {
    height: 64px;
    text-align: center;
  }

  .ports {
    margin-top: calc(var(--unit) * 9);
  }

  .sides {
    display: flex;
    gap: calc(var(--unit) * 2);
    margin-bottom: calc(var(--unit) * 4);
    border-bottom: var(--border-hairline) solid var(--line);
  }

  .sides button {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 5);
    border: none;
    border-bottom: 2px solid transparent;
    background: none;
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
    cursor: pointer;
  }

  .sides button[aria-selected="true"] {
    border-bottom-color: var(--accent);
    color: var(--text);
  }

  .port.rejected {
    color: var(--waiting);
  }

  .port.error {
    color: var(--failed);
  }

  .refused {
    color: var(--failed);
  }
</style>
