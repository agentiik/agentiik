<script lang="ts">
  import { explain, Told, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { Refusal, type API, type Me } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Pane from "../components/Pane.svelte";
  import Notice from "../components/Notice.svelte";
  import ProfileForm from "../components/ProfileForm.svelte";
  import ServiceAccounts from "../components/ServiceAccounts.svelte";
  import {
    addPasskey,
    credentialsOf,
    described,
    expiringIn,
    listOf,
    mint,
    policyOf,
    removeCredential,
    removeGenerator,
    revoke,
    scopeOf,
    serviceAccountsOf,
    SignInAgain,
    tokensOf,
    type Credential,
    type Issued,
    type Policy,
    type ServiceAccount,
    type Token,
    type TokenRequest,
  } from "../lib/credentials";
  import { clock } from "../lib/format";
  import { follow, type Place } from "../lib/place.svelte";
  import { sentence, signInWithPasskey, signInWithPassword } from "../lib/signin";
  import type { Passkeys } from "./SignIn.svelte";

  // The caller's own account: what they sign in with, and the API tokens they and the service
  // accounts of the namespaces they own hold. Every rule is the API's, which the screen says as the
  // API answered it: a removal the policy's minimum keeps, the sign-in in the last 10 minutes adding
  // a way in takes, a token's longest life. changed is what the console does once the session may
  // have gone, as it does with the password that opened it.
  let {
    api,
    place,
    me,
    tab,
    passkeys,
    changed,
  }: { api: API; place: Place; me: Me; tab: string | undefined; passkeys: Passkeys; changed: () => Promise<void> } = $props();

  const shown = $derived(tab === "profile" || tab === "tokens" || tab === "service-accounts" ? tab : "credentials");
  const now = Date.now();

  // What any act on the screen is doing, said, and what the API refused, said as it said it.
  let working = $state(false);
  let said = $state("");
  let problem = $state<Explained | null>(null);

  async function act(failed: string, work: () => Promise<void>) {
    if (working) {
      return;
    }
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

  // The sign-in methods, and the policy that rules them.
  let credentials = $state<Credential[] | null>(null);
  let policy = $state<Policy | null>(null);
  let unread = $state<Explained | null>(null);
  let untokened = $state<Explained | null>(null);

  async function reread() {
    try {
      credentials = await credentialsOf(api);
      unread = null;
    } catch (e) {
      // A credential removed ends the sessions it opened, this one among them where it did.
      if (e instanceof Refusal && e.status === 401) {
        await changed();
        return;
      }
      unread = explain("load your passkeys and passwords", e);
    }
  }

  $effect(() => {
    if (shown === "credentials") {
      void reread();
      policyOf(api).then(
        (p) => (policy = p),
        () => (policy = null),
      );
    }
  });

  // asking is the credential whose removal waits on a second click, so that one click removes nothing.
  let asking = $state("");
  let code = $state("");

  function remove(c: Credential) {
    return act("remove the credential", async () => {
      await removeCredential(api, c.id);
      asking = "";
      said = c.type === "password" ? "Password removed." : `${described(c)} removed.`;
      await reread();
    });
  }

  function removeTheGenerator(event: SubmitEvent) {
    event.preventDefault();
    return act("remove the one-time code generator", async () => {
      await removeGenerator(api, code.trim());
      code = "";
      said = "One-time code generator removed.";
      await reread();
    });
  }

  // Adding a passkey, and signing in again first where the session is too old to add one.
  let label = $state("");
  let again = $state(false);
  let password = $state("");
  let totp = $state("");
  const holdsPassword = $derived(credentials?.some((c) => c.type === "password") ?? false);

  function add(event?: SubmitEvent) {
    event?.preventDefault();
    return act("add the passkey", async () => {
      if (!passkeys.credentials) {
        throw new Told(passkeys.unavailable);
      }
      said = "Waiting for your authenticator.";
      try {
        const made = await addPasskey(api, passkeys.credentials, label);
        again = false;
        label = "";
        said = "Passkey added.";
      } catch (e) {
        said = "";
        if (e instanceof SignInAgain) {
          again = true;
        }
        throw e;
      }
      await reread();
    });
  }

  function signInAgainWithPasskey() {
    return act("sign in again with your passkey", async () => {
      if (!passkeys.credentials) {
        throw new Told(passkeys.unavailable);
      }
      said = "Waiting for your passkey.";
      await signInWithPasskey(api, passkeys.credentials);
      said = "Signed in again. Adding the passkey now.";
      working = false;
      await add();
    });
  }

  function signInAgainWithPassword(event: SubmitEvent) {
    event.preventDefault();
    return act("sign in again with your password", async () => {
      await signInWithPassword(api, me.user?.login ?? me.principal, password, totp.trim());
      password = "";
      totp = "";
      said = "Signed in again. Adding the passkey now.";
      working = false;
      await add();
    });
  }

  function policyLine(p: Policy): string {
    const parts = [`The installation keeps at least ${p.min_passkeys} ${p.min_passkeys === 1 ? "passkey" : "passkeys"} on an account before its password can go, and removes none below that.`];
    if (p.device_bound_only) {
      parts.push("Synced passkeys sign nobody in here, and count for none.");
    }
    if (p.password === "forbidden") {
      parts.push("Passwords are forbidden.");
    }
    return parts.join(" ");
  }

  // The tokens, and minting one.
  let tokens = $state<Token[] | null>(null);
  let accounts = $state<ServiceAccount[]>([]);
  let issued = $state<Issued | null>(null);
  let copied = $state(false);
  let revoking = $state("");

  async function retokens() {
    try {
      tokens = await tokensOf(api);
      untokened = null;
    } catch (e) {
      untokened = explain("load your tokens", e);
    }
  }

  $effect(() => {
    if (shown === "tokens") {
      void retokens();
      serviceAccountsOf(api).then(
        (a) => (accounts = a),
        () => (accounts = []),
      );
    }
  });

  const permissions = ["workflow:read", "workflow:run", "workflow:write", "workflow:delete", "run:read", "run:read_data", "secret:use", "secret:write", "grant:manage"] as const;
  type Permission = (typeof permissions)[number];

  let whose = $state("");
  let deviceLabel = $state("");
  let days = $state(90);
  let kept = $state<Permission[]>([]);
  let within = $state("");

  function mintOne(event: SubmitEvent) {
    event.preventDefault();
    return act("create the token", async () => {
      const ask: TokenRequest = { expires_at: expiringIn(days, new Date()) };
      if (whose !== "") {
        ask.principal = whose;
      }
      if (deviceLabel.trim() !== "") {
        ask.device_label = deviceLabel.trim();
      }
      const reach = listOf(within);
      if (kept.length > 0 || reach.length > 0) {
        ask.scope = {};
        if (kept.length > 0) {
          ask.scope.permissions = [...kept];
        }
        if (reach.length > 0) {
          ask.scope.within = reach;
        }
      }
      issued = await mint(api, ask);
      copied = false;
      deviceLabel = "";
      kept = [];
      within = "";
      await retokens();
    });
  }

  async function copy() {
    if (!issued) {
      return;
    }
    await navigator.clipboard.writeText(issued.token);
    copied = true;
  }

  function revokeOne(t: Token) {
    return act("revoke the token", async () => {
      await revoke(api, t.id);
      revoking = "";
      said = "Token revoked.";
      await retokens();
    });
  }

  function toggle(p: Permission, on: boolean) {
    kept = on ? [...kept, p] : kept.filter((x) => x !== p);
  }

  const routes = {
    profile: { kind: "account" as const, tab: "profile" },
    credentials: { kind: "account" as const },
    tokens: { kind: "account" as const, tab: "tokens" },
    accounts: { kind: "account" as const, tab: "service-accounts" },
  };
</script>

<PageHeader title="Your account" icon="control-users" {place} tabs={[
  { label: "Profile", icon: "control-users", to: routes.profile, current: shown === "profile" },
  { label: "Sign-in methods", icon: "control-passkey", to: routes.credentials, current: shown === "credentials" },
  { label: "API tokens", icon: "control-copy", to: routes.tokens, current: shown === "tokens" },
  { label: "Service accounts", icon: "control-groups", to: routes.accounts, current: shown === "service-accounts" },
]} />

{#if problem}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if said}{#key said}<Notice ondismiss={() => (said = "")}>{said}</Notice>{/key}{/if}

{#if shown === "profile"}
  <ProfileForm {api} {me} reread={changed} />
{:else if shown === "service-accounts"}
  <ServiceAccounts {api} {me} />
{:else if shown === "credentials"}
  <div class="columns">
    <Pane title="Sign-in methods" aside={credentials ? String(credentials.length) : ""}>
      {#if policy}<p class="muted lead">{policyLine(policy)}</p>{/if}
      {#if unread}
        <Problem explained={unread} onretry={reread} />
      {:else if credentials === null}
        <p class="muted">Loading</p>
      {:else if credentials.length === 0}
        <p class="muted">Tokens only</p>
      {:else}
        <table>
          <thead><tr><th>Credential</th><th>Kind</th><th>Enrolled</th><th>Last used</th><th class="end"></th></tr></thead>
          <tbody>
            {#each credentials as c (c.id)}
              <tr>
                <td>{#if c.type === "passkey"}<Icon name="control-passkey" size={14} />{/if}{described(c)}</td>
                <td class="muted">{c.type === "passkey" ? c.kind : c.type === "password" ? "password" : "one-time codes"}</td>
                <td class="term muted"><time datetime={c.created_at} title={c.created_at}>{clock(c.created_at, now)}</time></td>
                <td class="term muted">{#if c.last_used_at}<time datetime={c.last_used_at} title={c.last_used_at}>{clock(c.last_used_at, now)}</time>{:else}not used yet{/if}</td>
                <td class="end">
                  {#if c.type === "totp"}
                    <form class="inline" onsubmit={removeTheGenerator}>
                      <label class="unseen" for="generator-code">A code the generator shows now</label>
                      <input id="generator-code" class="code term" inputmode="numeric" autocomplete="one-time-code" placeholder="code it shows" maxlength="6" bind:value={code} />
                      <button class="control" disabled={working || code.trim().length !== 6}>Remove</button>
                    </form>
                  {:else if asking === c.id}
                    <span class="confirm">
                      <button class="control danger" disabled={working} onclick={() => remove(c)}>Remove {c.type === "password" ? "the password" : "it"}</button>
                      <button class="control" onclick={() => (asking = "")}>Keep it</button>
                    </span>
                  {:else}
                    <button class="control" onclick={() => (asking = c.id)}><Icon name="control-remove" size={14} />Remove</button>
                  {/if}
                </td>
              </tr>
              {#if asking === c.id}
                <tr class="asked"><td colspan="5" class="muted">{c.type === "password" ? "The password signs nobody in from now on, the generator beside it goes too, and the sessions it opened end." : "This passkey signs nobody in from now on, and the sessions it opened end."}</td></tr>
              {/if}
            {/each}
          </tbody>
        </table>
      {/if}
    </Pane>

    <Pane title="Add a passkey">
      {#if passkeys.unavailable}
        <p class="muted">{passkeys.unavailable}</p>
      {:else}
        <form onsubmit={add}>
          <label for="passkey-label">Name</label>
          <input id="passkey-label" maxlength="256" placeholder="work laptop" bind:value={label} />
          <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Add a passkey</button></p>
        </form>
        {#if again}
          <div class="again">
            <p>Sign in again to add a passkey.</p>
            <p><button class="control" disabled={working} onclick={signInAgainWithPasskey}><Icon name="control-passkey" size={14} />Sign in again with a passkey</button></p>
            {#if holdsPassword}
              <form onsubmit={signInAgainWithPassword}>
                <label for="again-password">Or with your password</label>
                <input id="again-password" type="password" autocomplete="current-password" bind:value={password} />
                <label for="again-totp">One-time code</label>
                <input id="again-totp" class="term" inputmode="numeric" autocomplete="one-time-code" maxlength="6" bind:value={totp} />
                <p><button class="control" disabled={working || password === ""}>Sign in again with the password</button></p>
              </form>
            {/if}
          </div>
        {/if}
      {/if}
    </Pane>
  </div>
{:else}
  <div class="columns">
    <Pane title="API tokens" aside={tokens ? String(tokens.length) : ""}>
      {#if issued}
        <div class="issued" role="status">
          <p>Token for <span class="term">{issued.api_token.principal}</span>. Shown once: copy it now.</p>
          <p class="value code">{issued.token}</p>
          <p class="buttons">
            <button class="control" onclick={copy}><Icon name="control-copy" size={14} />{copied ? "Copied" : "Copy"}</button>
            <button class="control" onclick={() => (issued = null)}>Done</button>
          </p>
        </div>
      {/if}
      {#if untokened}
        <Problem explained={untokened} onretry={retokens} />
      {:else if tokens === null}
        <p class="muted">Loading</p>
      {:else if tokens.length === 0}
        <p class="muted">No tokens</p>
      {:else}
        <table>
          <thead><tr><th>Token</th><th>Principal</th><th>Narrowed to</th><th>Created</th><th>Expires</th><th>Last used</th><th class="end"></th></tr></thead>
          <tbody>
            {#each tokens as t (t.id)}
              <tr>
                <td>{#if t.device_label}{t.device_label}{:else}<span class="code muted">{t.id}</span>{/if}</td>
                <td class="term">{t.principal}</td>
                <td class="muted">{scopeOf(t)}</td>
                <td class="term muted"><time datetime={t.created_at} title={t.created_at}>{clock(t.created_at, now)}</time></td>
                <td class="term muted"><time datetime={t.expires_at} title={t.expires_at}>{clock(t.expires_at, now)}</time></td>
                <td class="term muted">{#if t.last_used_at}<time datetime={t.last_used_at} title={t.last_used_at}>{clock(t.last_used_at, now)}</time>{:else}not used yet{/if}</td>
                <td class="end">
                  {#if revoking === t.id}
                    <span class="confirm">
                      <button class="control danger" disabled={working} onclick={() => revokeOne(t)}>Revoke it</button>
                      <button class="control" onclick={() => (revoking = "")}>Keep it</button>
                    </span>
                  {:else}
                    <button class="control" onclick={() => (revoking = t.id)}>Revoke</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </Pane>

    <Pane title="Mint a token">
      <form onsubmit={mintOne}>
        <label for="token-for">For</label>
        <select id="token-for" bind:value={whose}>
          <option value="">you, {me.principal}</option>
          {#each accounts as a (`${a.namespace}/${a.name}`)}
            <option value={`${a.namespace}/${a.name}`}>{a.namespace}/{a.name}</option>
          {/each}
        </select>
        <label for="token-label">Label</label>
        <input id="token-label" maxlength="256" placeholder="deploy pipeline" bind:value={deviceLabel} />
        <label for="token-days">Expires in</label>
        <select id="token-days" bind:value={days}>
          {#each [7, 30, 90, 180, 365] as d (d)}
            <option value={d}>{d} days</option>
          {/each}
        </select>
        <fieldset>
          <legend>Permissions (all if none ticked)</legend>
          {#each permissions as p (p)}
            <label class="check"><input type="checkbox" checked={kept.includes(p)} onchange={(e) => toggle(p, e.currentTarget.checked)} /><span class="term">{p}</span></label>
          {/each}
        </fieldset>
        <label for="token-within">Scope (everywhere if empty)</label>
        <input id="token-within" class="term" placeholder="finance, finance/monthly-invoicing" bind:value={within} />
        <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Mint the token</button></p>
      </form>
    </Pane>
  </div>
{/if}

<style>

  .columns {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(320px, 1fr);
    gap: calc(var(--unit) * 8);
    align-items: start;
  }



  .lead {
    margin: 0 0 calc(var(--unit) * 5);
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
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
    vertical-align: middle;
  }

  td :global(svg) {
    margin-right: calc(var(--unit) * 2);
    vertical-align: -2px;
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  tr.asked td {
    box-shadow: none;
    padding-top: 0;
    text-align: right;
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
    gap: calc(var(--unit) * 2);
  }

  form.inline {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
  }

  label {
    margin-top: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  input:not([type="checkbox"]),
  select {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
  }

  input.code {
    width: 11ch;
  }

  fieldset {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: calc(var(--unit) * 1) calc(var(--unit) * 4);
    margin: calc(var(--unit) * 3) 0 0;
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-control);
  }

  legend {
    padding: 0 calc(var(--unit) * 2);
    color: var(--muted);
    font-size: var(--type-control-size);
  }

  label.check {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 2);
    margin: 0;
  }

  form p,
  .again p {
    margin: calc(var(--unit) * 3) 0 0;
  }

  .again {
    margin-top: calc(var(--unit) * 6);
    padding-top: calc(var(--unit) * 4);
    border-top: var(--border-hairline) solid var(--line);
  }

  .issued {
    margin-bottom: calc(var(--unit) * 6);
    padding: calc(var(--unit) * 4) calc(var(--unit) * 5);
    border: var(--border-hairline) solid var(--accentLine);
    border-radius: var(--radius-control);
    background: var(--accentDim);
  }

  .issued p {
    margin: 0 0 calc(var(--unit) * 3);
  }

  .issued p:last-child {
    margin-bottom: 0;
  }

  .issued .buttons {
    display: flex;
    gap: calc(var(--unit) * 2);
  }

  .value {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    background: var(--sunken);
    word-break: break-all;
  }

  /* Under 1100px, where the sidebar folds, the two columns go one above the other. */
  @media (max-width: 1099px) {
    .columns {
      grid-template-columns: minmax(0, 1fr);
    }
  }

  @media (max-width: 759px) {
    fieldset {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
