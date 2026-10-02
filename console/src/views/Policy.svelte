<script lang="ts">
  import { refusal, type API } from "../api/client";
  import AdminTabs from "../components/AdminTabs.svelte";
  import Notice from "../components/Notice.svelte";
  import Pane from "../components/Pane.svelte";
  import Problem from "../components/Problem.svelte";
  import { policyOf, type Policy } from "../lib/credentials";
  import type { Place } from "../lib/place.svelte";
  import { explain, type Explained } from "../lib/problem";

  // The installation's sign-in policy, to an administrator: each setting by its own name, as the
  // policy and agk auth policy write it, with the two values it takes. PUT replaces the policy whole,
  // a setting left out returning to its default, so the form sends every setting every time. A change
  // that takes ways in away, passwords forbidden or synced passkeys refused, asks again before it is
  // sent, since the API deletes the passwords and suspends whoever is left with no way in, at once.
  let { api, place }: { api: API; place: Place } = $props();

  let stored = $state<Policy | null>(null);
  let unread = $state<Explained | null>(null);

  let password = $state<"allowed" | "forbidden">("allowed");
  let passkey = $state<"optional" | "required">("required");
  let verification = $state<"required" | "preferred">("required");
  let deviceBound = $state(false);
  let minimum = $state(2);

  function take(p: Policy) {
    stored = p;
    password = p.password ?? "allowed";
    passkey = p.passkey ?? "required";
    verification = p.user_verification ?? "required";
    deviceBound = p.device_bound_only ?? false;
    minimum = p.min_passkeys ?? 2;
  }

  async function read() {
    try {
      take(await policyOf(api));
      unread = null;
    } catch (e) {
      unread = explain("load the sign-in policy", e);
    }
  }

  $effect(() => {
    read();
  });

  const written = $derived<Policy>({ password, passkey, user_verification: verification, device_bound_only: deviceBound, min_passkeys: minimum });
  const changed = $derived(
    stored !== null &&
      (stored.password !== password || stored.passkey !== passkey || stored.user_verification !== verification || stored.device_bound_only !== deviceBound || stored.min_passkeys !== minimum),
  );
  // What the change takes away, which the second click is asked for.
  const takes = $derived(stored !== null && ((stored.password === "allowed" && password === "forbidden") || (!stored.device_bound_only && deviceBound)));

  let asking = $state(false);
  let working = $state(false);
  let problem = $state<Explained | null>(null);
  let said = $state("");

  function save(e: SubmitEvent) {
    e.preventDefault();
    if (!changed || working) return;
    if (takes && !asking) {
      asking = true;
      return;
    }
    working = true;
    problem = null;
    said = "";
    api
      .PUT("/api/v1/auth/policy", { body: written })
      .then(({ data, error, response }) => {
        if (!data) throw refusal(response, error);
        take(data);
        asking = false;
        said = "Policy saved.";
      })
      .catch((e: unknown) => (problem = explain("save the sign-in policy", e)))
      .finally(() => (working = false));
  }
</script>

<AdminTabs {place} current="policy" />

{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

<div class="policy">
  <Pane title="Policy">
    {#if unread}
      <Problem explained={unread} onretry={read} />
    {:else if stored === null}
      <p class="muted">Loading</p>
    {:else}
      <form onsubmit={save} aria-label="The installation's sign-in policy">
        <label class="field">
          <span class="term">password</span>
          <select bind:value={password} onchange={() => (asking = false)}>
            <option value="allowed">allowed</option>
            <option value="forbidden">forbidden</option>
          </select>
        </label>
        <label class="field">
          <span class="term">passkey</span>
          <select bind:value={passkey}>
            <option value="required">required</option>
            <option value="optional">optional</option>
          </select>
        </label>
        <label class="field">
          <span class="term">user_verification</span>
          <select bind:value={verification}>
            <option value="required">required</option>
            <option value="preferred">preferred</option>
          </select>
        </label>
        <label class="field">
          <span class="term">device_bound_only</span>
          <span class="check"><input type="checkbox" bind:checked={deviceBound} onchange={() => (asking = false)} />{deviceBound ? "true" : "false"}</span>
        </label>
        <label class="field">
          <span class="term">min_passkeys</span>
          <input type="number" min="1" step="1" bind:value={minimum} required />
        </label>
        {#if problem}<Problem explained={problem} />{/if}
        {#if asking}
          <p class="takes" role="alert">
            {#if password === "forbidden" && stored.password === "allowed"}Every stored password and one-time code generator is deleted, and every account left with no passkey the policy accepts is suspended.{/if}
            {#if deviceBound && !stored.device_bound_only}Synced passkeys sign nobody in from the next request.{/if}
          </p>
        {/if}
        <div class="field">
          <span></span>
          <span class="buttons">
            <button class="control primary" class:danger={asking} disabled={working || !changed}>Save</button>
            {#if asking}<button class="control" type="button" onclick={() => take(stored!)}>Keep</button>{/if}
          </span>
        </div>
      </form>
    {/if}
  </Pane>
</div>

<style>
  .policy {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: calc(var(--unit) * 8);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 6);
  }

  .field {
    display: grid;
    grid-template-columns: 180px minmax(0, 240px);
    align-items: center;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  .check {
    display: flex;
    align-items: center;
    gap: calc(var(--unit) * 3);
  }

  select,
  input[type="number"] {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    color: var(--text);
    font-size: var(--type-control-size);
  }

  .takes {
    margin: 0;
    max-width: 640px;
    color: var(--failed);
    font-size: var(--type-control-size);
  }

  .buttons {
    display: inline-flex;
    gap: calc(var(--unit) * 2);
  }

  .control.primary.danger {
    border-color: var(--failed);
    background: var(--failed);
  }

  @media (max-width: 759px) {
    .field {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
