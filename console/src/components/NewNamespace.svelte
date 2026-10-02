<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import { refusal, type API } from "../api/client";
  import Dialog from "./Dialog.svelte";
  import Icon from "./Icon.svelte";
  import Problem from "./Problem.svelte";

  // A shared namespace created by whoever asks, who owns it from the start: POST /api/v1/namespaces
  // gives its creator the owner role in the same transaction. Another owner and quotas are an
  // administrator's to set, from Namespaces.
  let { api, open = $bindable(false), created }: { api: API; open?: boolean; created: (name: string) => Promise<void> } = $props();

  let name = $state("");
  let working = $state(false);
  let problem = $state<Explained | null>(null);

  $effect(() => {
    if (open) {
      name = "";
      problem = null;
    }
  });

  async function create(e: SubmitEvent) {
    e.preventDefault();
    if (working) return;
    working = true;
    problem = null;
    const ask = name.trim();
    try {
      const { data, error, response } = await api.POST("/api/v1/namespaces", { body: { name: ask } });
      if (!data) throw refusal(response, error);
      open = false;
      await created(data.name);
    } catch (e) {
      problem = explain(`create ${ask}`, e);
    } finally {
      working = false;
    }
  }
</script>

<Dialog title="New namespace" bind:open>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={create} aria-label="New namespace">
    <label>
      <span>Name</span>
      <input class="term" bind:value={name} placeholder="accounting" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" spellcheck="false" autocomplete="off" />
    </label>
    <span class="buttons"><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Create</button></span>
  </form>
</Dialog>

<style>
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

  input {
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
