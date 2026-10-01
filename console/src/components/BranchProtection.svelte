<script lang="ts">
  import { explain, type Explained } from "../lib/problem";
  import Problem from "./Problem.svelte";
  import { refusal, type API } from "../api/client";
  import type { components } from "../api/schema";
  import Pane from "./Pane.svelte";

  // A workflow's default branch and its protection, on the workflow's sharing panel: both are
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
        said = `Saved. ${data.default_branch} is the default branch${data.protected ? ", and it is protected: only people with grant:manage can push to it" : ", and it is not protected: anyone with workflow:write can push to it"}.`;
      })
      .catch((e: unknown) => (problem = explain("save the default branch", e)))
      .finally(() => (working = false));
  }
</script>

<Pane title="Default branch" aside={`${namespace}/${workflow}`}>
  {#if unread}
    <Problem explained={unread} onretry={read} />
  {:else if repository === null}
    <p class="muted">Reading the workflow.</p>
  {:else}
    <form onsubmit={write} aria-label="Default branch of {namespace}/{workflow}">
      <label>
        <span>The branch a run uses when it is not given a ref</span>
        <input class="term" bind:value={branch} required autocomplete="off" spellcheck="false" />
      </label>
      <label class="check"><input type="checkbox" bind:checked={guarded} />Protected: only people with <span class="term">grant:manage</span> can push to it. Force-pushing or deleting it always needs <span class="term">grant:manage</span>, protected or not.</label>
      {#if problem}<Problem explained={problem} />{/if}
      {#if said}<p class="said" role="status">{said}</p>{/if}
      <p class="note muted">People with <span class="term">workflow:write</span> push to another branch, and a change reaches the default branch only through someone who can share the workflow. If you change the default branch, the protection moves to the new one and the old one is no longer protected.</p>
      <p><button class="control primary" disabled={working || !changed}>Save</button></p>
    </form>
  {/if}
</Pane>

<style>
  form {
    display: grid;
    gap: calc(var(--unit) * 4);
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
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  input:not([type="checkbox"]) {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  form p {
    margin: 0;
  }

  .note {
    font-size: var(--type-control-size);
  }
</style>
