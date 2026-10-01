<script lang="ts">
  import { refusal, type API, type Me } from "../api/client";
  import type { components } from "../api/schema";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import { holds } from "../lib/permissions";
  import { sentence } from "../lib/signin";

  // A namespace's settings, its secrets first: where each value is kept and where a step is given it,
  // never what it is. The declarations are shown to whoever reads the namespace's workflows, which is
  // what reading the workflows that name them already takes, and written by whoever holds
  // secret:write there. A value goes one way: it is written to the built-in store with its
  // declaration, no answer returns it, and rotating it is writing it again, never reading it first.
  let { api, me, namespace }: { api: API; me: Me; namespace: string } = $props();

  type Declaration = components["schemas"]["secretDeclaration"];
  type Store = Declaration["provider"];

  const reads = $derived(holds(me, "workflow:read", namespace));
  const writes = $derived(holds(me, "secret:write", namespace));

  let secrets = $state<Declaration[] | null>(null);
  let unread = $state("");

  async function read() {
    unread = "";
    const { data, error, response } = await api.GET("/api/v1/{ns}/secrets", { params: { path: { ns: namespace } } });
    if (data) secrets = data.secrets;
    else unread = refusal(response, error).message;
  }

  $effect(() => {
    if (reads) read();
  });

  let working = $state(false);
  let said = $state("");
  let problem = $state("");

  async function act(work: () => Promise<void>) {
    if (working) return;
    working = true;
    problem = "";
    said = "";
    try {
      await work();
    } catch (e) {
      problem = sentence(e instanceof Error ? e.message : String(e));
    } finally {
      working = false;
    }
  }

  // write declares a secret, or changes where it is kept, and for the built-in store writes its value.
  // The form is emptied as soon as the API has taken the value, so that it is held nowhere after.
  function write(name: string, body: { provider: Store; path?: string; value?: string; encoding?: "utf-8" | "base64" }, done: string) {
    return act(async () => {
      const { data, error, response } = await api.PUT("/api/v1/{ns}/secrets/{name}", { params: { path: { ns: namespace, name } }, body });
      if (!data) throw refusal(response, error);
      reset();
      said = done;
      await read();
    });
  }

  function reset() {
    name = "";
    path = "";
    value = "";
    base64 = false;
    rotating = "";
  }

  // The form that declares a secret, the same that rotates one, its name then given.
  let name = $state("");
  let store = $state<Store>("builtin");
  let path = $state("");
  let value = $state("");
  let base64 = $state(false);
  let rotating = $state("");

  function declare(e: SubmitEvent) {
    e.preventDefault();
    const n = (rotating || name).trim();
    const body =
      store === "builtin"
        ? { provider: store, ...(value ? { value, ...(base64 ? { encoding: "base64" as const } : {}) } : {}) }
        : { provider: store, path: path.trim() };
    const done = rotating
      ? `The value of ${n} is written. It is shown nowhere, and a run started from now reads it.`
      : store === "builtin" && value
        ? `${n} is declared and its value written. It is shown nowhere.`
        : `${n} is declared, kept in ${store}.`;
    return write(n, body, done);
  }

  function rotate(d: Declaration) {
    rotating = d.name;
    store = "builtin";
    value = "";
    base64 = false;
  }

  // asking is the declaration whose removal waits on a second click.
  let asking = $state("");

  function remove(d: Declaration) {
    return act(async () => {
      const answer = await api.DELETE("/api/v1/{ns}/secrets/{name}", { params: { path: { ns: namespace, name: d.name } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      said = `${d.name} is removed${d.provider === "builtin" ? ", its value with it" : ""}: a run naming it is refused from now on.`;
      asking = "";
      await read();
    });
  }

  const stores: { store: Store; label: string }[] = [
    { store: "builtin", label: "builtin, the encrypted store" },
    { store: "env", label: "env, the API's environment, for development" },
  ];
</script>

{#if problem}<p class="problem" role="alert">{problem}</p>{/if}
{#if said}<p class="said" role="status">{said}</p>{/if}

<div class="columns" class:alone={!(reads && writes)}>
  <Pane title="Secrets" aside={secrets ? `${secrets.length} in ${namespace}` : namespace}>
    {#if !reads}
      <p class="muted">The secrets a namespace declares are shown to whoever reads its workflows, workflow:read at namespace scope, which you do not hold in {namespace}.</p>
    {:else if unread}
      <p class="problem" role="alert">The secrets could not be read: {unread}</p>
    {:else if secrets === null}
      <p class="muted">Reading the secrets.</p>
    {:else}
      <table>
        <thead><tr><th>Name</th><th>Kept in</th><th>Given to a step at</th><th>Declared</th><th class="end"></th></tr></thead>
        <tbody>
          {#each secrets as d (d.name)}
            <tr>
              <td class="mono">{d.name}</td>
              <td>
                <span class="mono">{d.provider}</span>
                {#if d.path}<span class="mono muted path">{d.path}</span>{:else if d.provider === "builtin"}<span class="muted path">under {namespace} and its name</span>{/if}
              </td>
              <td class="mono muted">{d.mount}</td>
              <td class="muted nowrap"><span class="mono">{d.declared_by}</span> <time class="mono" datetime={d.declared_at} title={d.declared_at}>{d.declared_at.slice(0, 10)}</time></td>
              <td class="end">
                {#if writes}
                  {#if asking === d.name}
                    <span class="confirm">
                      <button class="control danger" disabled={working} onclick={() => remove(d)}>Remove it</button>
                      <button class="control" onclick={() => (asking = "")}>Keep it</button>
                    </span>
                  {:else}
                    {#if d.provider === "builtin"}<button class="control" onclick={() => rotate(d)}><Icon name="control-replay" size={14} />Rotate</button>{/if}
                    <button class="control" onclick={() => (asking = d.name)}><Icon name="control-remove" size={14} />Remove</button>
                  {/if}
                {/if}
              </td>
            </tr>
          {:else}
            <tr><td colspan="5" class="muted">{namespace} declares no secret.</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </Pane>

  {#if reads && writes}
    <Pane title={rotating ? `Rotate ${rotating}` : "Declare a secret"} focused={rotating !== ""}>
      <form onsubmit={declare} aria-label={rotating ? `Rotate ${rotating}` : "Declare a secret"}>
        {#if !rotating}
          <label>
            <span>Name</span>
            <input class="mono" bind:value={name} placeholder="stripe-key" required pattern="[A-Za-z0-9][A-Za-z0-9_\-]*" maxlength="255" />
          </label>
          <label>
            <span>Kept in</span>
            <select bind:value={store}>
              {#each stores as s (s.store)}<option value={s.store}>{s.label}</option>{/each}
            </select>
          </label>
        {/if}
        {#if store === "env"}
          <label>
            <span>Variable</span>
            <input class="mono" bind:value={path} placeholder="AGK_DEV_{namespace.toUpperCase().replaceAll('-', '_')}_STRIPE_KEY" required />
          </label>
        {:else}
          <label>
            <span>Value</span>
            <textarea class="mono" bind:value rows="3" spellcheck="false" autocomplete="off" required={rotating !== ""}></textarea>
          </label>
          <label class="check"><input type="checkbox" bind:checked={base64} />Written as base64, for a value that is not text</label>
        {/if}
        <p class="muted note">
          {#if store === "builtin"}A value is written once and shown nowhere, here or in any answer; rotating it is writing it again.{#if !rotating}{" "}Left empty, the declaration is written and a value kept before is kept.{/if}{:else}The API reads the variable from its own environment, under the prefix this installation gives {namespace}, and only where it gives one.{/if}
        </p>
        <span class="buttons">
          <button class="control primary" disabled={working}>{rotating ? "Write the value" : "Declare it"}</button>
          {#if rotating}<button class="control" type="button" onclick={() => (rotating = "")}>Keep the value it has</button>{/if}
        </span>
      </form>
    </Pane>
  {/if}
</div>

<style>
  .columns {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(320px, 1fr);
    gap: calc(var(--unit) * 8);
    align-items: start;
  }

  .columns.alone {
    grid-template-columns: minmax(0, 1fr);
  }

  .problem {
    color: var(--failed);
  }

  .said,
  .problem {
    margin: 0 0 calc(var(--unit) * 6);
  }

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
    border-top: var(--border-hairline) solid var(--line);
    vertical-align: middle;
  }

  td :global(svg) {
    margin-right: calc(var(--unit) * 2);
    vertical-align: -2px;
  }

  .path {
    display: block;
    font-size: var(--type-identifier-size-min);
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

  label > span:first-child {
    color: var(--muted);
  }

  label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  input:not([type="checkbox"]),
  select,
  textarea {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .note {
    margin: 0;
    font-size: var(--type-control-size);
  }

  .buttons {
    display: flex;
    gap: calc(var(--unit) * 3);
  }
</style>
