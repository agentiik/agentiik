<script lang="ts">
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
  let unread = $state("");
  let working = $state(false);
  let problem = $state("");

  // What was issued last, for whom, shown until it is put away.
  let issued = $state<{ login: string; what: "recovery" | "enrolment"; link: string; code?: string; expires_at: string } | null>(null);
  let copied = $state(false);

  function reread() {
    return usersOf(api).then(
      (u) => {
        users = u;
        unread = "";
      },
      (e: unknown) => (unread = sentence(e instanceof Error ? e.message : String(e))),
    );
  }

  $effect(() => {
    reread();
  });

  async function act(work: () => Promise<void>) {
    if (working) {
      return;
    }
    working = true;
    problem = "";
    removed = "";
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

  // The form that adds a user, in a dialog opened from the screen's head.
  let adding = $state(false);
  let login = $state("");
  let displayName = $state("");
  let admin = $state(false);

  function add(e: SubmitEvent) {
    e.preventDefault();
    return act(async () => {
      const body = { login: login.trim(), ...(displayName.trim() ? { display_name: displayName.trim() } : {}), ...(admin ? { admin: true } : {}) };
      const { data, error, response } = await api.POST("/api/v1/users", { body });
      if (!data) throw refusal(response, error);
      issued = { login: data.user.login, what: "enrolment", link: data.enrolment.link, expires_at: data.enrolment.expires_at };
      copied = false;
      adding = false;
      login = "";
      displayName = "";
      admin = false;
      await reread();
    });
  }

  // unphoto removes a photo that should not be shown: an administrator's one say over a profile,
  // which its user otherwise writes alone.
  function unphoto(u: User) {
    return act(async () => {
      const answer = await api.DELETE("/api/v1/users/{login}/avatar", { params: { path: { login: u.login } } });
      if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
      await reread();
    });
  }

  // asking is the user whose removal waits on a second click, and removed the one removed last.
  let asking = $state("");
  let removed = $state("");

  function remove(u: User) {
    return act(async () => {
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
</script>

<AdminTabs {place} current="users">
  {#snippet actions()}
    <button class="control primary" onclick={() => ((adding = true), (problem = ""))}><Icon name="control-add" size={14} />Add a user</button>
  {/snippet}
</AdminTabs>

{#if problem && !adding}<Notice kind="problem" ondismiss={() => (problem = "")}>{problem}</Notice>{/if}
{#if removed}{#key removed}<Notice ondismiss={() => (removed = "")}><span class="term">{removed}</span> is removed, with their credentials, tokens, sessions, memberships and grants.</Notice>{/key}{/if}

<Dialog title={issued ? `${issued.what === "recovery" ? "Recovery code" : "Enrolment link"} for ${issued.login}` : ""} open={issued !== null} onclose={() => (issued = null)}>
  {#if issued}
    <div class="issued" role="status">
      <p>
        Shown this once and good until <time class="term" datetime={issued.expires_at}>{clock(issued.expires_at, now)}</time>. Hand it over yourself: it is never sent by mail.
      </p>
      <p class="value code">{issued.link}</p>
      {#if issued.code}<p class="muted">Or the code alone, typed on the enrolment page: <span class="code">{issued.code}</span></p>{/if}
      <p class="buttons">
        <button class="control" onclick={copy}><Icon name="control-copy" size={14} />{copied ? "Copied" : "Copy the link"}</button>
        <button class="control primary" onclick={() => (issued = null)}>Done</button>
      </p>
    </div>
  {/if}
</Dialog>

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
            <td class="term nowrap"><span class="who"><Avatar name={u.display_name} src={userPhotoOf(u)} size={24} /><span class="login">{u.login}</span></span></td>
            <td>{u.display_name}{#if u.title}<span class="muted title">{u.title}</span>{/if}</td>
            <td class="muted">
              {#if u.suspended}<span class="suspended">suspended{u.suspended_for === "no_passkey" ? ", holding no passkey the policy accepts" : ""}</span>{:else if u.admin}administrator{:else}user{/if}
            </td>
            <td class="term muted nowrap">{#if u.created_at}<time datetime={u.created_at} title={u.created_at}>{clock(u.created_at, now)}</time>{/if}</td>
            <td class="term muted nowrap">{#if u.last_sign_in_at}<time datetime={u.last_sign_in_at} title={u.last_sign_in_at}>{clock(u.last_sign_in_at, now)}</time>{:else}never{/if}</td>
            <td class="end">
              {#if u.login === own}
                <span class="faint">Another administrator issues yours</span>
              {:else}
                {#if asking === u.login}
                  <button class="control danger" disabled={working} onclick={() => remove(u)}>Remove {u.login}</button>
                  <button class="control" onclick={() => (asking = "")}>Keep</button>
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
        {/each}
      </tbody>
    </table>
    <p class="foot muted">An enrolment link enrols a first credential. A recovery code replaces lost ones, revokes the code the user held open, and is recorded with both your identities; it is never issued for your own account, since a session taken from you would otherwise give it a way in nobody vouched for.</p>
  {/if}
</Pane>

<Dialog title="Add a user" bind:open={adding}>
  {#if problem}<p class="problem" role="alert">{problem}</p>{/if}
  <form onsubmit={add} aria-label="Add a user">
    <label>
      <span>Login</span>
      <input class="term" bind:value={login} placeholder="dana" required pattern="[a-z0-9]+(-[a-z0-9]+)*" maxlength="255" autocomplete="off" />
    </label>
    <label>
      <span>Display name</span>
      <input bind:value={displayName} placeholder="Dana Okafor" maxlength="256" autocomplete="off" />
    </label>
    <label class="check"><input type="checkbox" bind:checked={admin} />An administrator of the installation</label>
    <p class="foot muted">The user is created with no credential, and its personal namespace with it. Their enrolment link is shown once, here: hand it over yourself, and they enrol what signs them in.</p>
    <p><button class="control primary" disabled={working}><Icon name="control-add" size={14} />Add the user</button></p>
  </form>
</Dialog>

<style>
  .problem {
    margin: 0 0 calc(var(--unit) * 6);
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

  .foot {
    margin: calc(var(--unit) * 6) 0 0;
    font-size: var(--type-control-size);
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
