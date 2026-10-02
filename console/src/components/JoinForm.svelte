<script lang="ts">
  import { refusal, type API } from "../api/client";
  import { joinCommand, joinEnvironment, type Pool } from "../lib/fleet";
  import { clock } from "../lib/format";
  import { explain, type Explained } from "../lib/problem";
  import Icon from "./Icon.svelte";
  import Problem from "./Problem.svelte";

  // A runner added to a pool: a join token issued with POST /api/v1/runner-pools/{pool}/join-tokens
  // for the labels ticked among the pool's, then shown once with what redeems it on the host, the
  // command run as root or the environment a container starts with. A host that has joined before is
  // given --replace, which joins it as a new runner: an identity cannot follow a change to the labels
  // the API checked when they were claimed, so changing a runner's labels is replacing it.
  let {
    api,
    base,
    pools,
    pool = $bindable(),
    ticked = $bindable(),
    replace = false,
    ondone,
  }: { api: API; base: string; pools: Pool[]; pool: string; ticked: string[]; replace?: boolean; ondone: () => void } = $props();

  type Issued = { token: string; labels: string[]; expires_at: string };
  let issued = $state<Issued | null>(null);
  let working = $state(false);
  let problem = $state<Explained | null>(null);
  let copied = $state<"command" | "environment" | null>(null);

  const chosen = $derived(pools.find((p) => p.name === pool));

  // A pool chosen again ticks every label it grants, which is what a runner of it usually claims.
  function choose(name: string) {
    pool = name;
    ticked = [...(pools.find((p) => p.name === name)?.labels ?? [])];
  }

  async function issue(e: SubmitEvent) {
    e.preventDefault();
    if (!chosen) return;
    working = true;
    problem = null;
    const { data, response, error } = await api.POST("/api/v1/runner-pools/{pool}/join-tokens", { params: { path: { pool: chosen.name } }, body: { labels: chosen.labels.filter((l) => ticked.includes(l)) } });
    working = false;
    const token = data?.join_token;
    if (!token?.token) {
      problem = explain("issue a join token", refusal(response, error));
      return;
    }
    issued = { token: token.token, labels: token.labels, expires_at: token.expires_at };
    copied = null;
  }

  async function copy(what: "command" | "environment", text: string) {
    await navigator.clipboard.writeText(text);
    copied = what;
  }
</script>

{#if issued}
  {@const command = joinCommand(base, issued.token, issued.labels, replace)}
  {@const environment = joinEnvironment(base, issued.token, issued.labels)}
  <div class="issued" role="status">
    <p>Shown once. Expires <time class="term" datetime={issued.expires_at} title={issued.expires_at}>{clock(issued.expires_at, Date.now())}</time>.</p>
    <h3>On the host, as root</h3>
    <pre class="value code">{command}</pre>
    <p class="buttons"><button class="control" onclick={() => copy("command", command)}><Icon name={copied === "command" ? "state-succeeded" : "control-copy"} size={14} />Copy</button></p>
    <h3>In a container's environment</h3>
    <pre class="value code">{environment}</pre>
    <p class="buttons">
      <button class="control" onclick={() => copy("environment", environment)}><Icon name={copied === "environment" ? "state-succeeded" : "control-copy"} size={14} />Copy</button>
      <button class="control primary" onclick={ondone}>Done</button>
    </p>
  </div>
{:else}
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={issue} aria-label="Add a runner">
    <label>
      <span>Pool</span>
      <select value={pool} onchange={(e) => choose(e.currentTarget.value)} disabled={replace}>
        {#each pools as p (p.name)}<option value={p.name}>{p.name}</option>{/each}
      </select>
    </label>
    <fieldset>
      <legend>Labels</legend>
      {#if chosen && chosen.labels.length}
        <div class="labels">
          {#each chosen.labels as l (l)}
            <label class="check"><input type="checkbox" value={l} bind:group={ticked} /><span class="term">{l}</span></label>
          {/each}
        </div>
      {:else}
        <span class="muted">No label</span>
      {/if}
    </fieldset>
    <p class="buttons"><button class="control primary" disabled={working || !chosen}><Icon name="control-add" size={14} />Issue a join token</button></p>
  </form>
{/if}

<style>
  form {
    display: grid;
    gap: calc(var(--unit) * 6);
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
    margin: 0;
    padding: 0;
    border: none;
  }

  legend {
    margin-bottom: calc(var(--unit) * 3);
    padding: 0;
  }

  .labels {
    display: flex;
    flex-wrap: wrap;
    gap: calc(var(--unit) * 2) calc(var(--unit) * 8);
  }

  label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  .buttons {
    display: flex;
    justify-content: flex-end;
    gap: calc(var(--unit) * 2);
    margin: 0;
  }

  .issued p {
    margin: 0 0 calc(var(--unit) * 4);
    font-size: var(--type-control-size);
  }

  .issued .buttons {
    margin-bottom: calc(var(--unit) * 6);
  }

  .issued .buttons:last-child {
    margin-bottom: 0;
  }

  h3 {
    margin: 0 0 calc(var(--unit) * 3);
    color: var(--muted);
    font-size: var(--type-control-size);
    font-weight: 500;
  }

  .value {
    margin: 0 0 calc(var(--unit) * 3);
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    background: var(--sunken);
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
