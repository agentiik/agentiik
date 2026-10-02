<script lang="ts">
  import Filter from "../components/Filter.svelte";
  import { filtered } from "../lib/palette";
  import { explain, refused as refusedHere, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import type { Place } from "../lib/place.svelte";
  import { follow } from "../lib/place.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import { refusal, type API, type Me } from "../api/client";
  import type { components } from "../api/schema";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import Notice from "../components/Notice.svelte";
  import { holds } from "../lib/permissions";
  import { shown, parsed } from "../lib/variables";

  // A namespace's variables: each by its name, with its value as JSON, the workflows that read it and
  // who wrote it last. They are read by whoever reads the namespace's workflows, since a variable
  // serves every workflow it is shown to, and written by whoever may push to them, since a variable
  // changes what they do as a push does: workflow:read and workflow:write at the namespace's scope.
  let { api, place, me, namespace }: { api: API; place: Place; me: Me; namespace: string } = $props();

  type Variable = components["schemas"]["namespaceVariable"];
  type Visibility = Variable["visibility"];

  const reads = $derived(holds(me, "workflow:read", namespace));
  const writes = $derived(holds(me, "workflow:write", namespace));

  let variables = $state<Variable[] | null>(null);
  let unread = $state<Explained | null>(null);

  async function read() {
    unread = null;
    const { data, error, response } = await api.GET("/api/v1/{ns}/variables", { params: { path: { ns: namespace } } });
    if (data) variables = data.variables;
    else unread = explain("load the variables", refusal(response, error));
  }

  $effect(() => {
    if (reads) read();
  });

  // The namespace's workflows a variable can be shown to. The API lists no namespace's workflows, so
  // these are those its runs name and those a variable already names; one not pushed yet is typed.
  let ran = $state<string[]>([]);

  async function readWorkflows() {
    const { data } = await api.GET("/api/v1/runs", { params: { query: { namespace, limit: 200 } } });
    if (data) ran = [...new Set(data.runs.filter((r) => r.namespace === namespace).map((r) => r.workflow))];
  }

  $effect(() => {
    if (reads && writes) readWorkflows();
  });

  let working = $state(false);
  let said = $state("");
  let problem = $state<Explained | null>(null);

  async function act(failed: string, work: () => Promise<void>) {
    if (working) return;
    working = true;
    problem = null;
    said = "";
    try {
      await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  // The form that creates a variable, the same that edits one, its name then given.
  let name = $state("");
  let value = $state("");
  let visibility = $state<Visibility>("all");
  let chosen = $state<string[]>([]);
  let typed = $state("");
  let editing = $state("");
  let creating = $state(false);
  const writing = $derived(creating || editing !== "");
  const workflows = $derived([...new Set([...ran, ...(variables ?? []).flatMap((v) => v.workflows ?? []), ...chosen])].sort());

  function create() {
    name = "";
    value = "";
    visibility = "all";
    chosen = [];
    typed = "";
    problem = null;
    creating = true;
  }

  function edit(v: Variable) {
    value = JSON.stringify(v.value, null, 2);
    visibility = v.visibility;
    chosen = [...(v.workflows ?? [])];
    typed = "";
    problem = null;
    editing = v.name;
  }

  function close() {
    creating = false;
    editing = "";
  }

  function add() {
    const w = typed.trim();
    if (w && !chosen.includes(w)) chosen = [...chosen, w];
    typed = "";
  }

  // save sends the variable whole, its visibility and for selected its workflows each time, so that
  // changing the value alone never widens it to every workflow. The value is held to JSON here,
  // before anything is sent, which is how 30 is a number and "30" a string.
  function save(e: SubmitEvent) {
    e.preventDefault();
    const n = (editing || name).trim();
    const json = parsed(value);
    if (!json.ok) {
      problem = refusedHere(`save ${n}`, json.why);
      return;
    }
    const body = visibility === "selected" ? { value: json.value, visibility, workflows: chosen } : { value: json.value, visibility };
    const done = editing ? `${n} updated.` : `${n} saved.`;
    return act(`save ${n}`, async () => {
      const { data, error, response } = await api.PUT("/api/v1/{ns}/variables/{name}", { params: { path: { ns: namespace, name: n } }, body });
      if (!data) throw refusal(response, error);
      close();
      said = done;
      await read();
    });
  }

  // asking is the variable whose removal waits on a second click.
  let asking = $state("");

  function remove(v: Variable) {
    return act(`remove ${v.name}`, async () => {
      const answer = await api.DELETE("/api/v1/{ns}/variables/{name}", { params: { path: { ns: namespace, name: v.name } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      said = `${v.name} removed.`;
      asking = "";
      await read();
    });
  }

  const choices: { visibility: Visibility; label: string }[] = [
    { visibility: "all", label: "All workflows" },
    { visibility: "selected", label: "Selected workflows" },
  ];
  // What the Filter field at the head leaves of the list, as typed into the address.
  const variablesShown = $derived(variables ? filtered(variables, place.query.get("q") ?? "", (v) => v.name) : []);
</script>

<PageHeader title="Variables" icon="control-variables" count={variables?.length} {place}>
  {#snippet actions()}
    <Filter {place} label="Filter the variables" />
    {#if reads && writes}
      <button class="control primary" onclick={create}><Icon name="control-add" size={14} />New variable</button>
    {/if}
  {/snippet}
</PageHeader>

{#if problem && !writing}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<Pane title="" label="Variables">
  {#if !reads}
    <p class="muted">Hidden (needs <span class="term">workflow:read</span>)</p>
  {:else if unread}
    <Problem explained={unread} onretry={read} />
  {:else if variables === null}
    <p class="muted">Loading</p>
  {:else}
    <div class="scroll">
      <table>
        <thead><tr><th>Name</th><th>Value</th><th>Read by</th><th>Updated</th><th class="end"></th></tr></thead>
        <tbody>
          {#each variablesShown as v (v.name)}
            {@const json = shown(v.value)}
            <tr>
              <td class="term">{v.name}</td>
              <td class="value"><code class="code" title={json}>{json}</code></td>
              <td class="readers">
                {#if v.visibility === "all"}
                  <span>All workflows</span>
                {:else if (v.workflows ?? []).length === 0}
                  <span class="muted">None</span>
                {:else}
                  {#each v.workflows ?? [] as w, i (w)}
                    {@const to = { kind: "namespace" as const, namespace, view: "workflows" as const, workflow: w }}
                    {#if i > 0},{" "}{/if}<a href={place.href(to)} onclick={follow(place, to)}>{w}</a>
                  {/each}
                {/if}
              </td>
              <td class="muted nowrap"><span class="term">{v.updated_by}</span> <time class="term" datetime={v.updated_at} title={v.updated_at}>{v.updated_at.slice(0, 10)}</time></td>
              <td class="end">
                {#if writes}
                  {#if asking === v.name}
                    <span class="confirm">
                      <button class="control" onclick={() => (asking = "")}>Keep</button>
                      <button class="control danger" disabled={working} onclick={() => remove(v)}><Icon name="control-remove" size={14} />Remove</button>
                    </span>
                  {:else}
                    <button class="control" onclick={() => edit(v)}><Icon name="control-edit" size={14} />Edit</button>
                    <button class="control" onclick={() => (asking = v.name)}><Icon name="control-remove" size={14} />Remove</button>
                  {/if}
                {/if}
              </td>
            </tr>
          {:else}
            <tr><td colspan="5" class="muted">{#if variables.length === 0}{namespace} has no variable.{:else}Nothing matches.{/if}</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</Pane>

{#if reads && writes}
  <Dialog title={editing ? `Edit ${editing}` : "New variable"} open={writing} width={560} onclose={close}>
    {#if problem}<Problem explained={problem} />{/if}
    <form onsubmit={save} aria-label={editing ? `Edit ${editing}` : "New variable"}>
      {#if !editing}
        <label>
          <span>Name</span>
          <input class="term" bind:value={name} placeholder="ledger_url" required pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" />
        </label>
      {/if}
      <label>
        <span>Value, as JSON</span>
        <textarea class="code" bind:value rows="5" spellcheck="false" autocomplete="off" placeholder={'"https://ledger.example.com/api"'} required></textarea>
      </label>
      <fieldset>
        <legend>Read by</legend>
        <div class="choices">
          {#each choices as c (c.visibility)}
            <label class="check"><input type="radio" name="visibility" value={c.visibility} bind:group={visibility} />{c.label}</label>
          {/each}
        </div>
      </fieldset>
      {#if visibility === "selected"}
        <fieldset>
          <legend>Workflows</legend>
          {#if workflows.length}
            <div class="choices">
              {#each workflows as w (w)}
                <label class="check"><input type="checkbox" value={w} bind:group={chosen} />{w}</label>
              {/each}
            </div>
          {/if}
          <span class="another">
            <input class="term" bind:value={typed} aria-label="Another workflow" placeholder="quarterly-close" pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" onkeydown={(e) => { if (e.key === "Enter") { e.preventDefault(); add(); } }} />
            <button class="control" type="button" onclick={add}><Icon name="control-add" size={14} />Add</button>
          </span>
        </fieldset>
      {/if}
      <span class="buttons">
        <button class="control primary" disabled={working}>Save</button>
      </span>
    </form>
  </Dialog>
{/if}

<style>
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--type-control-size);
  }

  th {
    padding: calc(var(--unit) * 2) calc(var(--unit) * 3);
    color: var(--muted);
    font-weight: 500;
    text-align: left;
  }

  td {
    padding: calc(var(--unit) * 3);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    vertical-align: middle;
  }

  td :global(svg) {
    margin-right: calc(var(--unit) * 2);
    vertical-align: -2px;
  }

  /* A value is any JSON value, a long one cut at the column's edge and whole under the pointer, so
     that one variable holding a list does not widen the table for every other. */
  .value {
    width: 40%;
    min-width: 160px;
    max-width: 0;
  }

  .value code {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .readers a {
    color: inherit;
  }

  .nowrap {
    white-space: nowrap;
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  .end .control + .control {
    margin-left: calc(var(--unit) * 2);
  }

  .confirm {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
  }

  .control.danger {
    border-color: var(--failed);
    color: var(--failed);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 5);
  }

  label {
    display: grid;
    gap: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  label > span:first-child,
  legend {
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  fieldset {
    display: grid;
    gap: calc(var(--unit) * 3);
    margin: 0;
    padding: 0;
    border: none;
  }

  legend {
    margin-bottom: calc(var(--unit) * 2);
    padding: 0;
  }

  .choices {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 8);
  }

  label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  /* A browser draws a radio with a margin above it alone, which would sit it under its label's line. */
  label.check input {
    margin: 0;
  }

  .another {
    display: flex;
    gap: calc(var(--unit) * 3);
  }

  .another input {
    flex: 1;
    min-width: 0;
  }

  input:not([type="checkbox"]):not([type="radio"]),
  textarea {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .buttons {
    display: flex;
    gap: calc(var(--unit) * 3);
  }
</style>
