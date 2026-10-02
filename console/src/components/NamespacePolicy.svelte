<script lang="ts">
  import { refusal, type API } from "../api/client";
  import { policyOf, type Policy } from "../lib/credentials";
  import { explain, type Explained } from "../lib/problem";
  import Pane from "./Pane.svelte";
  import Problem from "./Problem.svelte";

  // A namespace's tightening of the sign-in policy, in its settings: what applies to an account
  // holding a grant there is the stricter of this and the installation's policy, setting by setting.
  // Whoever holds a role in the namespace reads it, since it applies to them, and an administrator
  // writes it. A setting is offered only its values stricter than the installation's, since a looser
  // one is refused, and the installation's value where the namespace leaves it as it is.
  // framed draws it as a pane of the namespace's settings; unframed, as a section of the dialog an
  // administrator writes a namespace's quotas in, which reaches a namespace they hold no role in.
  let { api, namespace, admin, framed = true }: { api: API; namespace: string; admin: boolean; framed?: boolean } = $props();

  // The namespace's own settings, each left out where it keeps the installation's.
  type Own = Partial<Policy>;

  let installation = $state<Policy | null>(null);
  let own = $state<Own | null>(null);
  let unread = $state<Explained | null>(null);

  // Each setting as the form holds it, "" where the namespace keeps the installation's.
  let password = $state("");
  let passkey = $state("");
  let verification = $state("");
  let deviceBound = $state("");
  let minimum = $state("");

  function take(p: Own) {
    own = p;
    password = p.password ?? "";
    passkey = p.passkey ?? "";
    verification = p.user_verification ?? "";
    deviceBound = p.device_bound_only === undefined ? "" : String(p.device_bound_only);
    minimum = p.min_passkeys === undefined ? "" : String(p.min_passkeys);
  }

  async function read() {
    try {
      const [inst, mine] = await Promise.all([
        policyOf(api),
        api.GET("/api/v1/{ns}/auth/policy", { params: { path: { ns: namespace } } }).then(({ data, error, response }) => {
          if (!data) throw refusal(response, error);
          return data;
        }),
      ]);
      installation = inst;
      take(mine);
      unread = null;
    } catch (e) {
      unread = explain("load the namespace's sign-in policy", e);
    }
  }

  $effect(() => {
    void namespace;
    read();
  });

  // What the namespace writes: the settings it sets, and nothing for those it keeps as the
  // installation's, since a setting written is one it holds to.
  const written = $derived.by((): Own => {
    const p: Own = {};
    if (password) p.password = password as Policy["password"];
    if (passkey) p.passkey = passkey as Policy["passkey"];
    if (verification) p.user_verification = verification as Policy["user_verification"];
    if (deviceBound) p.device_bound_only = deviceBound === "true";
    if (minimum !== "") p.min_passkeys = Number(minimum);
    return p;
  });
  const changed = $derived(own !== null && JSON.stringify(written) !== JSON.stringify(own));

  let working = $state(false);
  let problem = $state<Explained | null>(null);
  let said = $state("");

  function save(e: SubmitEvent) {
    e.preventDefault();
    if (!changed || working) return;
    working = true;
    problem = null;
    said = "";
    api
      // The generated type writes every setting, as the installation's answer carries them all; a
      // namespace's body names the settings it tightens and no other.
      .PUT("/api/v1/{ns}/auth/policy", { params: { path: { ns: namespace } }, body: written as Policy })
      .then(({ data, error, response }) => {
        if (!data) throw refusal(response, error);
        take(data);
        said = "Policy saved.";
      })
      .catch((e: unknown) => (problem = explain("save the namespace's sign-in policy", e)))
      .finally(() => (working = false));
  }

  // What a setting reads where it is drawn and not written: the namespace's value, or the
  // installation's where it keeps it.
  const shown = (mine: unknown, inst: unknown) => (mine === undefined ? `${String(inst)}, as the installation` : String(mine));
</script>

{#snippet body()}
  {#if unread}
    <Problem explained={unread} onretry={read} />
  {:else if installation === null || own === null}
    <p class="muted">Loading</p>
  {:else if admin}
    <form onsubmit={save} aria-label="The namespace's sign-in policy">
      <label class="field">
        <span class="term">password</span>
        <select bind:value={password}>
          <option value="">{installation.password}, as the installation</option>
          {#if installation.password === "allowed"}<option value="forbidden">forbidden</option>{/if}
        </select>
      </label>
      <label class="field">
        <span class="term">passkey</span>
        <select bind:value={passkey}>
          <option value="">{installation.passkey}, as the installation</option>
          {#if installation.passkey === "optional"}<option value="required">required</option>{/if}
        </select>
      </label>
      <label class="field">
        <span class="term">user_verification</span>
        <select bind:value={verification}>
          <option value="">{installation.user_verification}, as the installation</option>
          {#if installation.user_verification === "preferred"}<option value="required">required</option>{/if}
        </select>
      </label>
      <label class="field">
        <span class="term">device_bound_only</span>
        <select bind:value={deviceBound}>
          <option value="">{String(installation.device_bound_only)}, as the installation</option>
          {#if !installation.device_bound_only}<option value="true">true</option>{/if}
        </select>
      </label>
      <label class="field">
        <span class="term">min_passkeys</span>
        <input type="number" min={installation.min_passkeys} step="1" bind:value={minimum} placeholder={`${installation.min_passkeys}, as the installation`} />
      </label>
      {#if problem}<Problem explained={problem} />{/if}
      {#if said}<p class="said" role="status">{said}</p>{/if}
      <div class="field">
        <span></span>
        <span><button class="control primary" disabled={working || !changed}>Save</button></span>
      </div>
    </form>
  {:else}
    <dl class="read">
      <div class="field"><dt class="term">password</dt><dd>{shown(own.password, installation.password)}</dd></div>
      <div class="field"><dt class="term">passkey</dt><dd>{shown(own.passkey, installation.passkey)}</dd></div>
      <div class="field"><dt class="term">user_verification</dt><dd>{shown(own.user_verification, installation.user_verification)}</dd></div>
      <div class="field"><dt class="term">device_bound_only</dt><dd>{shown(own.device_bound_only, installation.device_bound_only)}</dd></div>
      <div class="field"><dt class="term">min_passkeys</dt><dd>{shown(own.min_passkeys, installation.min_passkeys)}</dd></div>
    </dl>
  {/if}
{/snippet}

{#if framed}
  <Pane title="Sign-in policy">{@render body()}</Pane>
{:else}
  <section class="bare" aria-label="Sign-in policy">
    <h3>Sign-in policy</h3>
    {@render body()}
  </section>
{/if}

<style>
  .bare {
    display: grid;
    gap: calc(var(--unit) * 6);
    margin-top: calc(var(--unit) * 8);
    padding-top: calc(var(--unit) * 8);
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
  }

  h3 {
    margin: 0;
    font-size: var(--type-control-size);
    font-weight: 600;
  }

  form,
  .read {
    display: grid;
    gap: calc(var(--unit) * 6);
    margin: 0;
  }

  .field {
    display: grid;
    grid-template-columns: 180px minmax(0, 300px);
    align-items: center;
    gap: calc(var(--unit) * 3) calc(var(--unit) * 6);
    font-size: var(--type-control-size);
  }

  dd {
    margin: 0;
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

  .said {
    margin: 0;
  }

  @media (max-width: 759px) {
    .field {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
