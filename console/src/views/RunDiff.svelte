<script lang="ts">
  import PageHeader from "../components/PageHeader.svelte";
  import type { API, Me } from "../api/client";
  import Pane from "../components/Pane.svelte";
  import StatePill from "../components/StatePill.svelte";
  import Refused from "./Refused.svelte";
  import { readEnvelope, tokens, type Item } from "../lib/envelope";
  import { between, clock, took } from "../lib/format";
  import { holds } from "../lib/permissions";
  import { useKeys } from "../lib/keys.svelte";
  import { follow, type Place } from "../lib/place.svelte";
  import { canonical, diffSteps, exits, firstDifference, paramsOf, sameItems, summed } from "../lib/run-diff";
  import { RunReader, type RunDetail } from "../lib/run.svelte";

  // Two runs of one commit side by side, to read a failed run against a good one: where they part,
  // step by step, from what each run's record says, and under run:read_data what a step's ports held
  // and what it was dispatched with. Neither is read again while it runs: a diff is of two runs that
  // have ended, and the inspector is where a run is followed.
  let { api, place, me, namespace, a, b }: { api: API; place: Place; me: Me; namespace: string; a: string; b: string } = $props();

  const first = $derived(new RunReader(api, a));
  const second = $derived(new RunReader(api, b));

  $effect(() => {
    first.read();
    second.read();
  });

  const x = $derived(first.run);
  const y = $derived(second.run);
  const missing = $derived(first.missing || second.missing || (x !== null && x.namespace !== namespace) || (y !== null && y.namespace !== namespace));
  const readsData = $derived(x && y ? holds(me, "run:read_data", x.namespace, x.workflow) && holds(me, "run:read_data", y.namespace, y.workflow) : false);

  const steps = $derived(x && y ? diffSteps(x, y, readsData) : []);
  const parted = $derived(firstDifference(steps));
  const chosen = $derived.by(() => {
    const named = place.query.get("step");
    return steps.find((s) => s.step === named) ?? steps.find((s) => s.step === parted) ?? steps[0];
  });

  const sameInputs = $derived(x && y ? canonical(x.inputs ?? {}) === canonical(y.inputs ?? {}) : true);

  function choose(step: string) {
    const q = new URLSearchParams(place.query);
    q.set("step", step);
    place.narrow(q);
    compared = {};
  }

  const route = (run: string) => ({ kind: "namespace" as const, namespace, view: "runs" as const, run });
  const swapped = $derived({ kind: "namespace" as const, namespace, view: "runs" as const, run: b, against: a });

  // Back to the first run, whose inspector the comparison was opened from.
  useKeys(() => [{ keys: ["Escape"], effect: "Back to the first run", does: () => place.go({ kind: "namespace", namespace, view: "runs", run: a }) }]);

  function lasted(r: RunDetail | { started_at?: string; finished_at?: string } | undefined): string {
    const ms = between(r?.started_at, r?.finished_at, Date.now());
    return ms === undefined ? "" : took(ms);
  }

  // What each port held in the two runs, compared on asking, since an envelope is read whole to be
  // compared and may be large: as two bags of items, by their data and files and not their ids.
  type Compared = { reading: true } | { refused: string } | { same: number; onlyFirst: Item[]; onlySecond: Item[] };
  let compared = $state<Record<string, Compared>>({});

  async function compare(port: string) {
    if (!x || !y || !chosen) return;
    compared = { ...compared, [port]: { reading: true } };
    try {
      const [p, q] = await Promise.all([readEnvelope(api, x.run, chosen.step, port, "output"), readEnvelope(api, y.run, chosen.step, port, "output")]);
      compared = { ...compared, [port]: sameItems(p, q) };
    } catch (e) {
      compared = { ...compared, [port]: { refused: e instanceof Error ? e.message : String(e) } };
    }
  }

  const portsOf = $derived(chosen ? [...new Set([...Object.keys(chosen.a?.ports ?? {}), ...Object.keys(chosen.b?.ports ?? {}), ...(chosen.a?.output_ports ?? [])])] : []);
  const now = Date.now();
</script>

<PageHeader title="Two runs" icon="control-diff" {place} />

{#if missing}
  <Refused />
{:else if first.refused || second.refused}
  <Pane title="Two runs"><p class="refused" role="alert">The runs could not be read: {first.refused || second.refused}</p></Pane>
{:else if x && y}
  <div class="diff">
    <Pane title="Two runs of {x.workflow}" aside="{x.namespace}/{x.workflow}@{x.commit.slice(0, 7)}">
      <div class="pair">
        {#each [x, y] as r, i (r.run)}
          <div class="side">
            <span class="which muted">{i === 0 ? "First" : "Second"}</span>
            <StatePill state={r.state} />
            <a class="code id" href={place.href(route(r.run))} onclick={follow(place, route(r.run))}>{r.run}</a>
            <span class="muted">
              <span class="term">{r.trigger_kind}</span>
              · created <time class="term" datetime={r.created_at} title={r.created_at}>{clock(r.created_at, now)}</time>
              {#if r.started_at}· took <span class="term">{lasted(r)}</span>{/if}
              · by <span class="term">{r.triggered_by}</span>
            </span>
          </div>
        {/each}
        <a class="swap" href={place.href(swapped)} onclick={follow(place, swapped)}>Swap them</a>
      </div>
      {#if x.workflow !== y.workflow || x.commit !== y.commit}
        <p class="warning" role="note">These are runs of {x.workflow}@{x.commit.slice(0, 7)} and {y.workflow}@{y.commit.slice(0, 7)}: two runs are read side by side where they ran one commit, and a difference here may be the code's, which the diff of the two commits shows.</p>
      {/if}
      {#if readsData}
        {#if sameInputs}
          <p class="muted">Both were started with the same inputs.</p>
        {:else}
          <p>They were started with different inputs.</p>
          <div class="pair json">
            {#each [x, y] as r (r.run)}
              <pre class="json"><code>{#each tokens(r.inputs ?? {}) as t, i (i)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
            {/each}
          </div>
        {/if}
      {:else}
        <p class="faint">Their inputs and parameters are not compared: you do not hold run:read_data on {x.namespace}/{x.workflow}.</p>
      {/if}
    </Pane>

    <Pane title="Steps" aside={parted ? `${steps.filter((s) => s.differs.length > 0).length} of ${steps.length} differ; they part at ${parted}` : "no step differs"}>
      <table class="steps">
        <thead>
          <tr>
            <th>Step</th>
            <th>First run</th>
            <th class="number">Took</th>
            <th class="number">Exit</th>
            <th>Second run</th>
            <th class="number">Took</th>
            <th class="number">Exit</th>
            <th>What differs</th>
          </tr>
        </thead>
        <tbody>
          {#each steps as s (s.step)}
            <tr class:differs={s.differs.length > 0} class:chosen={s.step === chosen?.step}>
              <td>
                <button class="link term" aria-pressed={s.step === chosen?.step} onclick={() => choose(s.step)}>{s.step}</button>
                {#if s.step === parted}<span class="first">first difference</span>{/if}
              </td>
              <td>{#if s.a}<StatePill state={s.a.verdict} />{#if s.a.attempts > 1}<span class="muted term"> ×{s.a.attempts}</span>{/if}{/if}</td>
              <td class="number term">{lasted(s.a)}</td>
              <td class="number term">{s.a ? summed(exits(x, s.step)) : ""}</td>
              <td>{#if s.b}<StatePill state={s.b.verdict} />{#if s.b.attempts > 1}<span class="muted term"> ×{s.b.attempts}</span>{/if}{/if}</td>
              <td class="number term">{lasted(s.b)}</td>
              <td class="number term">{s.b ? summed(exits(y, s.step)) : ""}</td>
              <td>{s.differs.length ? s.differs.join(", ") : ""}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </Pane>

    {#if chosen}
      <Pane title="{chosen.step} in the two runs" aside={chosen.differs.length ? `differs in ${chosen.differs.join(", ")}` : "alike"}>
        <table class="ports">
          <thead>
            <tr><th>Output port</th><th class="number">First run</th><th class="number">Second run</th>{#if readsData}<th>Items</th>{/if}</tr>
          </thead>
          <tbody>
            {#each portsOf as port (port)}
              {@const p = chosen.a?.ports?.[port]}
              {@const q = chosen.b?.ports?.[port]}
              {@const c = compared[port]}
              <tr>
                <td class="term">{port}</td>
                <td class="number term">{p ? `${p.items} items` : "nothing"}</td>
                <td class="number term">{q ? `${q.items} items` : "nothing"}</td>
                {#if readsData}
                  <td>
                    {#if !p || !q}
                      <span class="muted">only one run published here</span>
                    {:else if p.items === 0 && q.items === 0}
                      <span class="muted">nothing in either</span>
                    {:else if p.purged_at || q.purged_at}
                      <span class="muted">purged with the run's retention</span>
                    {:else if !c}
                      <button class="control" onclick={() => compare(port)}>Compare the items</button>
                    {:else if "reading" in c}
                      <span class="muted" role="status">Reading both envelopes</span>
                    {:else if "refused" in c}
                      <span class="refused" role="alert">{c.refused}</span>
                    {:else if c.onlyFirst.length === 0 && c.onlySecond.length === 0}
                      <span role="status">The same {c.same} items</span>
                    {:else}
                      <span role="status">{c.same} the same, {c.onlyFirst.length} only in the first, {c.onlySecond.length} only in the second</span>
                    {/if}
                  </td>
                {/if}
              </tr>
              {#if c && "same" in c && (c.onlyFirst.length > 0 || c.onlySecond.length > 0)}
                <tr class="items">
                  <td colspan={readsData ? 4 : 3}>
                    <div class="pair json">
                      {#each [c.onlyFirst, c.onlySecond] as only, i (i)}
                        <div>
                          <p class="faint">Only in the {i === 0 ? "first" : "second"} run{only.length > 20 ? `, the first 20 of ${only.length}` : ""}</p>
                          <pre class="json"><code>{#each tokens(only.slice(0, 20).map((it) => it.data)) as t, k (k)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
                        </div>
                      {/each}
                    </div>
                  </td>
                </tr>
              {/if}
            {:else}
              <tr><td colspan={readsData ? 4 : 3} class="muted">The step published nothing in either run.</td></tr>
            {/each}
          </tbody>
        </table>
        {#if readsData}
          {@const pa = paramsOf(x, chosen.step)}
          {@const pb = paramsOf(y, chosen.step)}
          {#if canonical(pa ?? {}) === canonical(pb ?? {})}
            <p class="muted">It was dispatched with the same parameters in both.</p>
          {:else}
            <p>It was dispatched with different parameters.</p>
            <div class="pair json">
              {#each [pa, pb] as params, i (i)}
                <pre class="json"><code>{#each tokens(params ?? {}) as t, k (k)}<span class="t-{t.kind}">{t.text}</span>{/each}</code></pre>
              {/each}
            </div>
          {/if}
        {/if}
        <p class="open">
          {#each [x, y] as r, i (r.run)}
            {@const at = { kind: "namespace" as const, namespace, view: "runs" as const, run: r.run }}
            <a href={place.href(at) + `?step=${encodeURIComponent(chosen.step)}`} onclick={(e) => { if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; e.preventDefault(); place.go(at, false, new URLSearchParams({ step: chosen.step })); }}>Open {chosen.step} in the {i === 0 ? "first" : "second"} run</a>
          {/each}
        </p>
      </Pane>
    {/if}
  </div>
{/if}

<style>
  .diff {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 10);
  }

  .pair {
    display: grid;
    grid-template-columns: 1fr 1fr auto;
    align-items: start;
    gap: calc(var(--unit) * 8);
  }

  .pair.json {
    grid-template-columns: 1fr 1fr;
  }

  .side {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .which {
    width: 100%;
    font-size: var(--type-columnHead-size);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .id {
    color: var(--accent);
    font-size: var(--type-identifier-size-max);
  }

  .swap,
  .open a {
    color: var(--accent);
    font-size: var(--type-control-size);
  }

  .open {
    display: flex;
    gap: calc(var(--unit) * 10);
    margin: calc(var(--unit) * 6) 0 0;
  }

  .warning {
    padding: calc(var(--unit) * 4) calc(var(--unit) * 6);
    border-left: 2px solid var(--waiting);
    background: var(--sunken);
  }

  .refused {
    color: var(--failed);
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th,
  td {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    box-shadow: inset 0 calc(-1 * var(--border-hairline)) 0 var(--line);
    text-align: left;
    vertical-align: middle;
  }

  th {
    color: var(--faint);
    font-size: var(--type-columnHead-size);
    font-weight: var(--type-columnHead-weight);
    letter-spacing: var(--type-columnHead-tracking);
    text-transform: var(--type-columnHead-case);
  }

  .number {
    text-align: right;
  }

  tr.differs td:last-child {
    color: var(--waiting);
  }

  tr.chosen {
    background: var(--accentDim);
  }

  .link {
    padding: 0;
    border: none;
    background: none;
    color: var(--text);
    font-size: var(--type-identifier-size-max);
    cursor: pointer;
  }

  .link[aria-pressed="true"] {
    color: var(--accent);
  }

  .first {
    margin-left: calc(var(--unit) * 4);
    padding: 1px calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--waiting);
    border-radius: var(--radius-round);
    color: var(--waiting);
    font-size: 11px;
  }

  .items td {
    background: var(--sunken);
  }

  pre.json {
    max-height: 320px;
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

  /* On a phone the two runs go one above the other. */
  @media (max-width: 759px) {
    .pair,
    .pair.json {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
