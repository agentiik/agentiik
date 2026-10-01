<script lang="ts">
  import type { API, Me } from "../api/client";
  import Icon from "../components/Icon.svelte";
  import Pane from "../components/Pane.svelte";
  import { enrolmentFor, recoveryFor, usersOf, type User } from "../lib/credentials";
  import { clock } from "../lib/format";
  import { sentence } from "../lib/signin";

  // The users of the installation, for an administrator: who they are, whether each administers it,
  // whether the policy suspended them and when each last signed in, and the two ways back in an
  // administrator hands over: an enrolment link for a user who holds nothing yet, and a recovery code
  // for one who lost what signs them in. Each is shown once, as the API answers it once, and handed
  // over by whoever issued it, never sent by mail, which would put the account behind a mailbox.
  let { api, me }: { api: API; me: Me } = $props();

  const now = Date.now();
  let users = $state<User[] | null>(null);
  let unread = $state("");
  let working = $state(false);
  let problem = $state("");

  // What was issued last, for whom, shown until it is put away.
  let issued = $state<{ login: string; what: "recovery" | "enrolment"; link: string; code?: string; expires_at: string } | null>(null);
  let copied = $state(false);

  $effect(() => {
    usersOf(api).then(
      (u) => {
        users = u;
        unread = "";
      },
      (e: unknown) => (unread = sentence(e instanceof Error ? e.message : String(e))),
    );
  });

  async function act(work: () => Promise<void>) {
    if (working) {
      return;
    }
    working = true;
    problem = "";
    try {
      await work();
    } catch (e) {
      problem = sentence(e instanceof Error ? e.message : String(e));
    } finally {
      working = false;
    }
  }

  function recover(u: User) {
    return act(async () => {
      const r = await recoveryFor(api, u.login);
      issued = { login: u.login, what: "recovery", link: r.link, code: r.code, expires_at: r.expires_at };
      copied = false;
    });
  }

  function enrol(u: User) {
    return act(async () => {
      const r = await enrolmentFor(api, u.login);
      issued = { login: u.login, what: "enrolment", link: r.link, expires_at: r.expires_at };
      copied = false;
    });
  }

  async function copy() {
    if (!issued) {
      return;
    }
    await navigator.clipboard.writeText(issued.link);
    copied = true;
  }

  const own = $derived(me.user?.login ?? "");
</script>

{#if problem}<p class="problem" role="alert">{problem}</p>{/if}

{#if issued}
  <div class="issued" role="status">
    <p>
      {issued.what === "recovery" ? "A recovery code" : "An enrolment link"} for <span class="mono">{issued.login}</span>, shown this once and good until
      <time class="mono" datetime={issued.expires_at}>{clock(issued.expires_at, now)}</time>. Hand it over yourself: it is never sent by mail.
    </p>
    <p class="value mono">{issued.link}</p>
    {#if issued.code}<p class="muted">Or the code alone, typed on the enrolment page: <span class="mono">{issued.code}</span></p>{/if}
    <p class="buttons">
      <button class="control" onclick={copy}><Icon name="control-copy" size={14} />{copied ? "Copied" : "Copy the link"}</button>
      <button class="control" onclick={() => (issued = null)}>Done</button>
    </p>
  </div>
{/if}

<Pane title="Users" aside={users ? String(users.length) : ""}>
  {#if unread}
    <p class="problem" role="alert">{unread}</p>
  {:else if users === null}
    <p class="muted">Reading the users.</p>
  {:else}
    <table>
      <thead><tr><th>Login</th><th>Name</th><th>Standing</th><th>Created</th><th>Last signed in</th><th class="end"></th></tr></thead>
      <tbody>
        {#each users as u (u.login)}
          <tr>
            <td class="mono">{u.login}</td>
            <td>{u.display_name}</td>
            <td class="muted">
              {#if u.suspended}<span class="suspended">suspended{u.suspended_for === "no_passkey" ? ", holding no passkey the policy accepts" : ""}</span>{:else if u.admin}administrator{:else}user{/if}
            </td>
            <td class="mono muted">{#if u.created_at}<time datetime={u.created_at} title={u.created_at}>{clock(u.created_at, now)}</time>{/if}</td>
            <td class="mono muted">{#if u.last_sign_in_at}<time datetime={u.last_sign_in_at} title={u.last_sign_in_at}>{clock(u.last_sign_in_at, now)}</time>{:else}never{/if}</td>
            <td class="end">
              {#if u.login === own}
                <span class="faint">Another administrator issues yours</span>
              {:else}
                {#if !u.last_sign_in_at}
                  <button class="control" disabled={working} onclick={() => enrol(u)}>Enrolment link</button>
                {/if}
                <button class="control" disabled={working} onclick={() => recover(u)}>Recovery code</button>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="foot muted">An enrolment link enrols a first credential. A recovery code replaces lost ones, revokes the code the user held open, and is recorded with both your identities; it is never issued for your own account, since a session taken from you would otherwise give it a way in nobody vouched for.</p>
  {/if}
</Pane>

<style>
  .problem {
    margin: 0 0 calc(var(--unit) * 6);
    color: var(--failed);
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
  }

  .end {
    text-align: right;
    white-space: nowrap;
  }

  .end .control + .control {
    margin-left: calc(var(--unit) * 2);
  }

  .suspended {
    color: var(--waiting);
  }

  .foot {
    margin: calc(var(--unit) * 6) 0 0;
    font-size: var(--type-control-size);
  }

  .issued {
    margin-bottom: calc(var(--unit) * 8);
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
</style>
