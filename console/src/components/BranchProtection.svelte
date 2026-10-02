<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import Pane from "./Pane.svelte";

  // A workflow's default branch and its protection, in the workflow's settings: both are
  // grant:manage's, the permission that governs sharing, since who may move the branch production
  // runs is decided by whoever may share the workflow. A protected default branch takes grant:manage
  // to push to, force-push or delete; another branch takes workflow:write, so that changes reach
  // production through somebody who holds it. Protection moves with the default branch, since only
  // the default branch is protected.
  let { api, namespace, workflow }: { api: API; namespace: string; workflow: string } = $props();

  type Repository = components["schemas"]["workflowDetail"]["repository"];

  let repository = $state<Repository | null>(null);
  let unread = $state<Explained | null>(null);

  async function read() {
    const { data, error, response } = await api.GET("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } } });
    if (data) {
      repository = data.repository;
      branch = data.repository.default_branch;
      guarded = data.repository.protected;
      unread = null;
    } else unread = explain("load the workflow", refusal(response, error));
  }

  $effect(() => {
    read();
  });

  let branch = $state("");
  let guarded = $state(false);
  let working = $state(false);
  let problem = $state<Explained | null>(null);
  let said = $state("");

  const changed = $derived(repository !== null && (branch.trim() !== repository.default_branch || guarded !== repository.protected));

  function write(e: SubmitEvent) {
    e.preventDefault();
    if (!repository || !changed || working) return;
    // Only what changed is sent, each field being judged on its own and nothing it leaves out
    // changed: a body naming the branch it already has would be a rename of nothing.
    const body: { default_branch?: string; protected?: boolean } = {};
    if (branch.trim() !== repository.default_branch) body.default_branch = branch.trim();
    if (guarded !== repository.protected) body.protected = guarded;
    working = true;
    problem = null;
    said = "";
    api
      .PATCH("/api/v1/{ns}/workflows/{name}", { params: { path: { ns: namespace, name: workflow } }, body })
      .then(({ data, error, response }) => {
        if (!data) throw refusal(response, error);
        repository = data;
        branch = data.default_branch;
        guarded = data.protected;
        said = "Saved.";
      })
      .catch((e: unknown) => (problem = explain("save the default branch", e)))
      .finally(() => (working = false));
  }
</script>

<Pane title="Default branch">
  {#if unread}
    <Problem explained={unread} onretry={read} />
  {:else if repository === null}
    <p class="muted">Loading</p>
  {:else}
    <!-- Laid out as the settings' other sections are, a label beside each field. -->
    <form onsubmit={write} aria-label="Default branch of {namespace}/{workflow}">
      <label class="field">
        <span class="label">Branch</span>
        <input class="term" bind:value={branch} required autocomplete="off" spellcheck="false" />
      </label>
      <div class="field">
        <span class="label">Protection</span>
        <label class="check"><input type="checkbox" bind:checked={guarded} />Protected</label>
      </div>
      {#if problem}<Problem explained={problem} />{/if}
      {#if said}<p class="said" role="status">{said}</p>{/if}
      <div class="field">
        <span></span>
        <span><button class="control primary" disabled={working || !changed}>Save</button></span>
      </div>
    </form>
  {/if}
</Pane>

<style>
  form {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  .field {
    display: grid;
    grid-template-columns: 120px minmax(0, 420px);
    align-items: center;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .label {
    color: var(--muted);
  }

  .check {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  input:not([type="checkbox"]) {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .said {
    margin: 0;
  }

  @media (max-width: 759px) {
    .field {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
