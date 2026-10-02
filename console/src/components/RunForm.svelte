<script lang="ts">
  import { explain, refused as refusedHere, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { untrack } from "svelte";
  import type { Validator } from "@cfworker/json-schema";
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import type { Place } from "../lib/place.svelte";
  import { runAt } from "../lib/route";
  import { fieldOf, initial, problems, read, validator, type Field } from "../lib/run-form";
  import Icon from "./Icon.svelte";

  // A run of the workflow asked for by hand: a field for each input the version declares, typed as
  // its JSON Schema types it and checked against that schema as it is left and before the run is
  // asked for; and the ref to run, the default branch's head where none is named. The declaration
  // is read from GET /api/v1/{ns}/workflows/{name}/inputs, under workflow:run, which is what asking
  // for a run takes, with the files of the tree its schemas name: so that whoever may run the
  // workflow without reading it, an operator, is asked for what a run takes as anybody else is. An
  // input the API still refuses is pointed at as the API names it. Once started, the run's
  // inspector opens.
  // ref is the one the form opens on: the files' ref where it opens from them, empty for the head.
  let { api, place, namespace, workflow, ref: opensOn = "", onclose }: { api: API; place: Place; namespace: string; workflow: string; ref?: string; onclose: () => void } = $props();

  type Declaration = components["schemas"]["runInputs"];
  let declaration = $state<Declaration | null>(null);
  let unread = $state<Explained | null>(null);

  async function readDeclaration() {
    unread = null;
    const on = untrack(() => opensOn).trim();
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}/inputs", { params: { path: { ns: namespace, name: workflow }, query: on ? { ref: on } : {} } });
    if (data) declaration = data;
    else unread = explain(`read what ${workflow} takes`, refusal(response, error));
  }

  $effect(() => {
    readDeclaration();
  });

  const declared = $derived(Object.entries(declaration?.inputs ?? {}));
  const fields = $derived(declared.map(([name, d]) => fieldOf(name, d as { schema: unknown; required?: boolean; default?: unknown })));
  const commit = $derived(declaration?.commit ?? "");

  let raw = $state<Record<string, string | boolean>>({});
  let errors = $state<Record<string, string[]>>({});
  let ref = $state(untrack(() => opensOn));
  let sending = $state(false);
  let refused = $state<Explained | null>(null);
  let validators = $state<Record<string, Validator | undefined>>({});

  $effect(() => {
    raw = Object.fromEntries(fields.map((f) => [f.name, initial(f)]));
    // Compiled once the declaration is read, each file a schema names taken from what the route
    // answered beside it, as the API reads them from the version's tree.
    const files = declaration?.files ?? {};
    const file = async (path: string) => files[path];
    for (const [name, d] of declared) {
      validator((d as { schema: unknown }).schema, file).then((v) => (validators = { ...validators, [name]: v }));
    }
  });

  function check(f: Field): unknown {
    const r = read(f, raw[f.name] ?? "");
    if (r.error) {
      errors = { ...errors, [f.name]: [r.error] };
      return undefined;
    }
    const v = validators[f.name];
    const found = !r.empty && v ? problems(v, r.value) : [];
    errors = { ...errors, [f.name]: found };
    return r.empty ? undefined : r.value;
  }

  async function start(e: SubmitEvent) {
    e.preventDefault();
    refused = null;
    const inputs: Record<string, unknown> = {};
    for (const f of fields) {
      const value = check(f);
      if (value !== undefined) inputs[f.name] = value;
    }
    if (Object.values(errors).some((list) => list.length > 0)) {
      refused = refusedHere(`start ${workflow}`, "Some inputs are invalid.");
      return;
    }
    sending = true;
    const body = ref.trim() ? { ref: ref.trim(), inputs } : { inputs };
    const { data, error, response } = await api.POST("/api/v1/{ns}/workflows/{name}/runs", { params: { path: { ns: namespace, name: workflow } }, body });
    sending = false;
    if (data) {
      place.go(runAt(namespace, workflow, data.run));
      return;
    }
    const named = error as { input?: string; error?: string } | undefined;
    if (response.status === 422 && named?.input && fields.some((f) => f.name === named.input)) {
      errors = { ...errors, [named.input]: [named.error ?? "refused"] };
      refused = refusedHere(`start ${workflow}`, `The input ${named.input} is invalid.`);
      return;
    }
    refused = explain(`start ${workflow}`, refusal(response, error));
  }

  const id = (name: string) => `input-${name}`;
</script>

{#if unread}
  <Problem explained={unread} onretry={readDeclaration} />
{:else if declaration === null}
  <p class="muted">Loading</p>
{:else}
<form class="form" onsubmit={start} novalidate>
  {#each fields as f (f.name)}
    {@const wrong = (errors[f.name] ?? []).length > 0}
    <div class="field" class:wrong>
      <label for={id(f.name)}>
        <span class="term name">{f.name}</span>
        {#if f.required && !f.hasDefault}<span class="muted">required</span>{:else if f.hasDefault}<span class="muted">default <span class="term">{JSON.stringify(f.default)}</span></span>{:else}<span class="muted">optional</span>{/if}
      </label>
      {#if f.description}<p class="faint">{f.description}</p>{/if}
      {#if f.kind === "boolean"}
        <input id={id(f.name)} type="checkbox" checked={raw[f.name] === true} onchange={(e) => { raw[f.name] = e.currentTarget.checked; check(f); }} />
      {:else if f.kind === "enum"}
        <select id={id(f.name)} value={raw[f.name]} onchange={(e) => { raw[f.name] = e.currentTarget.value; check(f); }}>
          {#if !f.required || f.hasDefault}<option value="">none</option>{/if}
          {#each f.options ?? [] as o, i (i)}<option value={String(i)}>{JSON.stringify(o)}</option>{/each}
        </select>
      {:else if f.kind === "json"}
        <textarea id={id(f.name)} class="code" rows="5" spellcheck="false" aria-invalid={wrong} value={String(raw[f.name] ?? "")} oninput={(e) => (raw[f.name] = e.currentTarget.value)} onblur={() => check(f)}></textarea>
      {:else}
        <input id={id(f.name)} class="term" type={f.kind === "string" ? "text" : "number"} step={f.kind === "integer" ? "1" : "any"} aria-invalid={wrong} value={String(raw[f.name] ?? "")} oninput={(e) => (raw[f.name] = e.currentTarget.value)} onblur={() => check(f)} />
      {/if}
      {#each errors[f.name] ?? [] as problem, i (i)}<p class="problem" role="alert">{problem}</p>{/each}
    </div>
  {:else}
    <p class="muted">No inputs</p>
  {/each}
  <div class="field">
    <label for="run-ref"><span>Ref</span></label>
    <input id="run-ref" class="term" type="text" bind:value={ref} placeholder={commit.slice(0, 7)} />
  </div>
  <div class="actions">
    <button class="control primary" type="submit" disabled={sending}><Icon name="control-run" size={14} />Start the run</button>
    <button class="control" type="button" onclick={onclose}>Close</button>
    {#if refused}<Problem explained={refused} />{/if}
  </div>
</form>
{/if}

<style>
  .form {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 8);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: calc(var(--unit) * 2);
  }

  label {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .name {
    font-weight: 600;
  }

  .faint {
    margin: 0;
    font-size: var(--type-control-size);
  }

  input[type="text"],
  input[type="number"],
  select,
  textarea {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  input[type="checkbox"] {
    align-self: flex-start;
  }

  .wrong input,
  .wrong textarea,
  .wrong select {
    border-color: var(--failed);
  }

  .problem {
    margin: 0;
    color: var(--failed);
    font-size: var(--type-control-size);
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: calc(var(--unit) * 5);
  }
</style>
