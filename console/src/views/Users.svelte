<script lang="ts">
  import Filter from "../components/Filter.svelte";
  import { filtered } from "../lib/palette";
  import { explain, type Explained } from "../lib/problem";
  import Problem from "../components/Problem.svelte";
  import { refusal, type API, type Me } from "../api/client";
  import AdminTabs from "../components/AdminTabs.svelte";
  import Avatar from "../components/Avatar.svelte";
  import { userPhotoOf } from "../lib/profile";
  import Dialog from "../components/Dialog.svelte";
  import Icon from "../components/Icon.svelte";
  import Notice from "../components/Notice.svelte";
  import Pane from "../components/Pane.svelte";
  import { enrolmentFor, recoveryFor, usersOf, type User } from "../lib/credentials";
  import { clock } from "../lib/format";
  import type { Place } from "../lib/place.svelte";
  import { sentence } from "../lib/signin";

  // The users of the installation, for an administrator: who they are, whether each administers it,
  // whether the policy suspended them and when each last signed in, and the two ways back in an
  // administrator hands over: an enrolment link for a user who holds nothing yet, and a recovery code
  // for one who lost what signs them in. Each is shown once, as the API answers it once, and handed
  // over by whoever issued it, never sent by mail, which would put the account behind a mailbox.
  //
  // An administrator also adds a user, whose enrolment link the API answers with it, and removes one,
  // on a second click: the API refuses a removal that would leave somebody's work or a namespace's
  // ownership behind, and says what.
  let { api, place, me }: { api: API; place: Place; me: Me } = $props();

  const now = Date.now();
  let users = $state<User[] | null>(null);
  let unread = $state<Explained | null>(null);
  let working = $state(false);
  let problem = $state<Explained | null>(null);

  // What was issued last, for whom, shown until it is put away.
  let issued = $state<{ login: string; what: "recovery" | "enrolment"; link: string; code?: string; expires_at: string } | null>(null);
  let copied = $state(false);

  function reread() {
    return usersOf(api).then(
      (u) => {
        users = u;
        unread = null;
      },
      (e: unknown) => (unread = explain("load the users", e)),
    );
  }

  $effect(() => {
    reread();
  });

  async function act(failed: string, work: () => Promise<void>) {
    if (working) {
      return;
    }
    working = true;
    problem = null;
    removed = "";
    try {
      await work();
    } catch (e) {
      problem = explain(failed, e);
    } finally {
      working = false;
    }
  }

  function recover(u: User) {
    return act("create a recovery link", async () => {
      const r = await recoveryFor(api, u.login);
      issued = { login: u.login, what: "recovery", link: r.link, code: r.code, expires_at: r.expires_at };
      copied = false;
    });
  }

  function enrol(u: User) {
    return act("create an enrolment link", async () => {
      const r = await enrolmentFor(api, u.login);
      issued = { login: u.login, what: "enrolment", link: r.link, expires_at: r.expires_at };
      copied = false;
    });
  }

  // The form that adds a user, in a dialog opened from the screen's head.
  let adding = $state(false);
  let login = $state("");
  let givenName = $state("");
  let familyName = $state("");
  let email = $state("");
  let admin = $state(false);

  function add(e: SubmitEvent) {
    e.preventDefault();
    return act("add the user", async () => {
      const body = {
        login: login.trim(),
        ...(givenName.trim() ? { given_name: givenName.trim() } : {}),
        ...(familyName.trim() ? { family_name: familyName.trim() } : {}),
        ...(email.trim() ? { email: email.trim() } : {}),
        ...(admin ? { admin: true } : {}),
      };
      const { data, error, response } = await api.POST("/api/v1/users", { body });
      if (!data) throw refusal(response, error);
      issued = { login: data.user.login, what: "enrolment", link: data.enrolment.link, expires_at: data.enrolment.expires_at };
      copied = false;
      adding = false;
      login = "";
      givenName = "";
      familyName = "";
      email = "";
      admin = false;
      await reread();
    });
  }

  // The email address of a user, which an administrator gives with PATCH /api/v1/users/{login}: the
  // one thing of a user an administrator writes after creating them, the empty string removing it.
  let addressing = $state<User | null>(null);
  let address = $state("");

  function addressOf(u: User) {
    addressing = u;
    address = u.email ?? "";
    problem = null;
  }

  function give(e: SubmitEvent) {
    e.preventDefault();
    const u = addressing;
    if (!u) return;
    return act("set the email address", async () => {
      const { data, error, response } = await api.PATCH("/api/v1/users/{login}", { params: { path: { login: u.login } }, body: { email: address.trim() } });
      if (!data) throw refusal(response, error);
      addressing = null;
      await reread();
    });
  }

  // unphoto removes a photo that should not be shown: an administrator's one say over a profile,
  // which its user otherwise writes alone.
  function unphoto(u: User) {
    return act("remove the photo", async () => {
      const answer = await api.DELETE("/api/v1/users/{login}/avatar", { params: { path: { login: u.login } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      await reread();
    });
  }

  // asking is the user whose removal waits on a second click, and removed the one removed last.
  let asking = $state("");
  let removed = $state("");

  function remove(u: User) {
    return act("remove the user", async () => {
      const answer = await api.DELETE("/api/v1/users/{login}", { params: { path: { login: u.login } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      asking = "";
      removed = u.login;
      await reread();
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
  // What the Filter field at the head leaves of the list, as typed into the address.
  const usersShown = $derived(users ? filtered(users, place.query.get("q") ?? "", (u) => `${u.login} ${u.display_name} ${u.email}`) : []);
</script>

<AdminTabs {place} current="users">
  {#snippet actions()}
    <Filter {place} label="Filter the users" />
    <button class="control primary" onclick={() => ((adding = true), (problem = null))}><Icon name="control-add" size={14} />Add a user</button>
  {/snippet}
</AdminTabs>

{#if problem && !adding && !addressing}<Notice kind="problem" explained={problem} ondismiss={() => (problem = null)} />{/if}
{#if removed}{#key removed}<Notice ondismiss={() => (removed = "")}><span class="term">{removed}</span> removed.</Notice>{/key}{/if}

<Dialog title={issued ? `${issued.what === "recovery" ? "Recovery code" : "Enrolment link"} for ${issued.login}` : ""} open={issued !== null} onclose={() => (issued = null)}>
  {#if issued}
    <div class="issued" role="status">
      <p>
        Shown once. Expires <time class="term" datetime={issued.expires_at}>{clock(issued.expires_at, now)}</time>.
      </p>
      <p class="value code">{issued.link}</p>
      {#if issued.code}<p class="muted">Code: <span class="code">{issued.code}</span></p>{/if}
      <p class="buttons">
        <button class="control" onclick={copy}><Icon name={copied ? "state-succeeded" : "control-copy"} size={14} />Copy the link</button>
        <button class="control primary" onclick={() => (issued = null)}>Done</button>
      </p>
    </div>
  {/if}
</Dialog>

<Pane title="Users" aside={users ? String(users.length) : ""}>
  {#if unread}
    <Problem explained={unread} onretry={reread} />
  {:else if users === null}
    <p class="muted">Loading</p>
  {:else}
    <table>
      <thead><tr><th>Login</th><th>Name</th><th>Email</th><th>Standing</th><th>Created</th><th>Last signed in</th><th class="end"></th></tr></thead>
      <tbody>
        {#each usersShown as u (u.login)}
          <tr>
            <td class="term nowrap"><span class="who"><Avatar name={u.display_name} src={userPhotoOf(u)} size={24} /><span class="login">{u.login}</span></span></td>
            <td class="nowrap">{u.display_name}{#if u.title}<span class="muted title">{u.title}</span>{/if}</td>
            <td class="nowrap" class:muted={!u.email}>{u.email || "none"}</td>
            <td class="muted">
              {#if u.suspended}<span class="suspended">suspended{u.suspended_for === "no_passkey" ? " (no passkey)" : ""}</span>{:else if u.admin}administrator{:else}user{/if}
            </td>
            <td class="term muted nowrap">{#if u.created_at}<time datetime={u.created_at} title={u.created_at}>{clock(u.created_at, now)}</time>{/if}</td>
            <td class="term muted nowrap">{#if u.last_sign_in_at}<time datetime={u.last_sign_in_at} title={u.last_sign_in_at}>{clock(u.last_sign_in_at, now)}</time>{:else}never{/if}</td>
            <td class="end">
              {#if asking !== u.login}<button class="control" disabled={working} onclick={() => addressOf(u)}>Email</button>{/if}
              {#if u.login !== own}
                {#if asking === u.login}
                  <button class="control" onclick={() => (asking = "")}>Keep</button>
                  <button class="control danger" disabled={working} onclick={() => remove(u)}>Remove</button>
                {:else}
                  {#if !u.last_sign_in_at}
                    <button class="control" disabled={working} onclick={() => enrol(u)}>Enrolment link</button>
                  {/if}
                  <button class="control" disabled={working} onclick={() => recover(u)}>Recovery code</button>
                  {#if u.avatar_updated_at}<button class="control" disabled={working} onclick={() => unphoto(u)}>Remove the photo</button>{/if}
                  <button class="control" disabled={working} onclick={() => (asking = u.login)}>Remove</button>
                {/if}
              {/if}
            </td>
          </tr>
        {:else}
          <tr><td colspan="7" class="muted">{users.length === 0 ? "No users" : "Nothing matches."}</td></tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Pane>

<Dialog title="Add a user" bind:open={adding}>
  {#if problem}<Problem explained={problem} />{/if}
  <form onsubmit={add} aria-label="Add a user">
    <label>
      <span>Login</span>
      <input class="term" bind:value={login} placeholder="dana" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
    </label>
    <div class="pair">
      <label>
        <span>Given name</span>
        <input bind:value={givenName} placeholder="Dana" maxlength="128" autocomplete="off" />
      </label>
      <label>
        <span>Family name</span>
        <input bind:value={familyName} placeholder="Okafor" maxlength="128" autocomplete="off" />
      </label>
    </div>
    <label>
      <span>Email</span>
      <input type="email" bind:value={email} placeholder="dana@example.com" maxlength="254" autocomplete="off" />
    </label>
    <label class="check"><input type="checkbox" bind:checked={admin} />An administrator of the installation</label>
    <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Add the user</button></p>
  </form>
</Dialog>

<Dialog title="Email address" open={addressing !== null} onclose={() => (addressing = null)}>
  {#if addressing}
    {#if problem}<Problem explained={problem} />{/if}
    <form onsubmit={give} aria-label="Email address">
      <p class="term">{addressing.login}</p>
      <label>
        <span>Email</span>
        <input type="email" bind:value={address} placeholder="dana@example.com" maxlength="254" autocomplete="off" />
      </label>
      <p><button class="control primary" disabled={working}>Save</button></p>
    </form>
  {/if}
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

  label.check {
    display: inline-flex;
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

  .pair {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: calc(var(--unit) * 5);
  }

  .pair input {
    width: 100%;
    min-width: 0;
  }

  form p {
    margin: 0;
  }

  .control.danger {
    border-color: var(--failed);
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
    box-shadow: inset 0 var(--border-hairline) 0 var(--line);
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

  .who {
    display: inline-flex;
    align-items: center;
    gap: calc(var(--unit) * 4);
  }

  .title {
    display: block;
    font-size: 12.5px;
  }

  .suspended {
    color: var(--waiting);
  }

  .issued p {
    margin: 0 0 calc(var(--unit) * 4);
  }

  .issued p:last-child {
    margin-bottom: 0;
  }

  .issued .buttons {
    display: flex;
    justify-content: flex-end;
    gap: calc(var(--unit) * 2);
  }

  .value {
    padding: calc(var(--unit) * 3) calc(var(--unit) * 4);
    border-radius: var(--radius-control);
    background: var(--sunken);
    word-break: break-all;
  }
</style>
