<script lang="ts">
  import { tokens } from "../lib/envelope";
  import { inputPorts, scheduling, type Step } from "../lib/graph";
  import type { RunDetail } from "../lib/run.svelte";
  import { follow, type Place } from "../lib/place.svelte";
  import { runAt } from "../lib/route";

  // One step of the graph as its version resolved it, said for its kind: a brick by its image, its
  // release and the ports its manifest declares; a script by its base image, its shell and its
  // commands in the order they run; a call by the workflow it calls, where, and the run it started
  // where a run is shown. Then what decides when and how it runs, its parameters as written, and
  // where each setting came from where it extends a block.
  let { name, step, run, place, namespace }: { name: string; step: Step; run: RunDetail | null; place: Place; namespace: string } = $props();

  const child = $derived(run?.tasks.find((t) => t.step === name && t.called)?.called);
  const marks = $derived(scheduling(step));
  const ins = $derived(inputPorts(step));
  const called = $derived(step.workflow?.workflow.split("/"));
  const childRoute = $derived(child && called?.length === 2 ? runAt(called[0]!, called[1]!, child) : undefined);
</script>

<dl class="detail">
  <dt>Kind</dt>
  <dd>{step.kind === "brick" ? "a brick" : step.kind === "script" ? "a script, run in a base image" : "a call of another workflow"}</dd>

  {#if step.kind === "workflow" && step.workflow}
    <dt>Calls</dt>
    <dd class="term">{step.workflow.workflow}</dd>
    <dt>At</dt>
    <dd class="term">{step.workflow.ref ?? "its default branch's head when the call is made"}</dd>
    {#if childRoute && child}
      <dt>Child run</dt>
      <dd><a class="term" href={place.href(childRoute)} onclick={follow(place, childRoute)}>{child}</a></dd>
    {/if}
  {:else}
    <dt>Image</dt>
    <dd class="code wrap">{step.image}</dd>
    {#if step.brick}
      <dt>Brick</dt>
      <dd class="term">{step.brick.name} {step.brick.version}</dd>
    {/if}
  {/if}

  {#if step.kind === "script"}
    <dt>Shell</dt>
    <dd class="term">{(step.shell ?? []).join(" ")}</dd>
  {/if}

  <dt>Input ports</dt>
  <dd class="term">{ins.length ? ins.join(", ") : "none"}</dd>
  <dt>Output ports</dt>
  <dd class="term">{(step.outputs ?? []).length ? step.outputs!.join(", ") : "none"}</dd>

  {#if marks.length}
    <dt>Scheduling</dt>
    <dd class="term">{marks.join(" · ")}</dd>
  {/if}
  {#if step.timeout}
    <dt>Timeout</dt>
    <dd class="term">{step.timeout} a shard</dd>
  {/if}
  {#if step.runs_on?.length}
    <dt>Runs on</dt>
    <dd class="term">{step.runs_on.join(", ")}</dd>
  {/if}
  {#if step.network}
    <dt>Network</dt>
    <dd class="term">{step.network}{step.egress?.allow?.length ? `: ${step.egress.allow.join(", ")}` : ""}</dd>
  {/if}
  {#if step.secrets?.length}
    <dt>Secrets</dt>
    <dd class="term">{step.secrets.join(", ")}</dd>
  {/if}
  {#if step.extends?.length}
    <dt>Extends</dt>
    <dd class="term">{step.extends.join(", ")}</dd>
  {/if}
  <dt>Idempotent</dt>
  <dd class="term">{step.idempotent ? "yes, a lost task is handed out again" : "no, a lost task fails the step"}</dd>
</dl>

{#if step.kind === "script"}
  {#each [["before_script", step.before_script], ["script", step.script], ["after_script", step.after_script]] as [label, lines] (label)}
    {#if lines && (lines as string[]).length}
      <p class="label term">{label}</p>
      <pre class="code"><code>{(lines as string[]).join("\n")}</code></pre>
    {/if}
  {/each}
{/if}

{#if step.params && Object.keys(step.params).length}
  <p class="label">Parameters, as written</p>
  <pre class="code json"><code>{#each tokens(step.params) as t, i (i)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
{/if}

<style>
  .detail {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 8);
    margin: 0;
    font-size: var(--type-control-size);
  }

  dt {
    color: var(--muted);
  }

  dd {
    margin: 0;
  }

  .wrap {
    overflow-wrap: anywhere;
  }

  .label {
    margin: calc(var(--unit) * 8) 0 calc(var(--unit) * 3);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  .code {
    margin: 0;
    padding: calc(var(--unit) * 4) calc(var(--unit) * 5);
    overflow: auto;
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
    background: var(--sunken);
    font-family: var(--type-identifier-font);
    font-size: 12px;
    --leading: 1.55;
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
</style>
