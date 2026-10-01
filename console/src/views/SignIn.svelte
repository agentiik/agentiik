<script lang="ts" module>
  // What this browser offers for a passkey ceremony: why it can run none, where it cannot, and the
  // credentials container it runs one with.
  export type Passkeys = { unavailable: string; credentials?: CredentialsContainer };
</script>

<script lang="ts">
  import { Refusal, type API } from "../api/client";
  import Pane from "../components/Pane.svelte";
  import Problem from "../components/Problem.svelte";
  import { Told, type Explained } from "../lib/problem";
  import type { Session } from "../lib/session.svelte";
  import { explainSignIn, PasswordsForbidden, sentence, signInWithPasskey, signInWithPassword } from "../lib/signin";

  // Signing in, where the API knows no session: with a passkey, the intended way, and with a password
  // as the fallback an installation may forbid. What the policy allows is read only once signed in,
  // so the password form is offered to anybody and withdrawn once the installation refuses a password
  // by naming the setting; hiding it is no policy, and the API refuses a password all the same.
  let { api, session, passkeys }: { api: API; session: Session; passkeys: Passkeys } = $props();

  let working = $state(false);
  let status = $state("");
  let problem = $state<Explained | null>(null);
  let forbidden = $state(false);
  let unavailable = $state("");

  // Whether the password form is unfolded: from the start where no passkey can be used, once a
  // ceremony finds the installation runs none, and kept open across a refusal, so that the person
  // corrects what they typed rather than finding the form folded away.
  let unfolded = $derived(passkeys.unavailable !== "");

  let login = $state("");
  let password = $state("");
  let totp = $state("");

  async function run(how: "passkey" | "password", work: () => Promise<void>) {
    if (working) {
      return;
    }
    working = true;
    problem = null;
    try {
      await work();
    } catch (e) {
      status = "";
      if (e instanceof PasswordsForbidden) {
        forbidden = true;
      }
      problem = explainSignIn(how, e);
    } finally {
      working = false;
    }
  }

  function withPasskey() {
    return run("passkey", async () => {
      if (!passkeys.credentials) {
        throw new Told(passkeys.unavailable);
      }
      status = "Waiting for your passkey.";
      try {
        const who = await signInWithPasskey(api, passkeys.credentials);
        status = `Signed in as ${who}.`;
      } catch (e) {
        // An installation addressed by an IP address runs no ceremony at all: the password is then
        // the one way in, and the button is withdrawn with the reason.
        if (e instanceof Refusal && e.status === 409) {
          unavailable = sentence(e.message);
          unfolded = true;
          status = "";
          return;
        }
        throw e;
      }
      await session.read();
    });
  }

  function withPassword(event: SubmitEvent) {
    event.preventDefault();
    return run("password", async () => {
      status = "Signing in.";
      const answer = await signInWithPassword(api, login.trim(), password, totp.trim());
      password = "";
      totp = "";
      status = answer.session === "enrolment" ? "Signed in, but only to set up a passkey: you can do nothing else until you have one." : `Signed in as ${answer.login}.`;
      await session.read();
    });
  }

  const offline = $derived(unavailable || passkeys.unavailable);
</script>

<main class="alone">
  <Pane title="Sign in">
    <p>This browser is not signed in to this installation.</p>
    {#if offline}
      <p class="note">{offline}</p>
    {:else}
      <p>Sign in with a passkey you set up, on this device or on a phone nearby.</p>
      <p><button class="control primary" disabled={working} onclick={withPasskey}>Sign in with a passkey</button></p>
    {/if}

    {#if !forbidden}
      <details bind:open={unfolded}>
        <summary>{offline ? "With a password" : "Use a password instead"}</summary>
        <form onsubmit={withPassword}>
          <label for="login">Login</label>
          <input id="login" name="login" autocomplete="username" autocapitalize="none" spellcheck="false" required bind:value={login} />
          <label for="secret">Password</label>
          <input id="secret" name="password" type="password" autocomplete="current-password" required bind:value={password} />
          <label for="totp">One-time code <span class="muted">only if you set one up</span></label>
          <input id="totp" name="totp" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" bind:value={totp} />
          <p><button class="control" type="submit" disabled={working}>Sign in with a password</button></p>
        </form>
      </details>
    {/if}

    <p class="status" role="status" aria-live="polite">{status}</p>
    {#if problem}<Problem explained={problem} />{/if}

    <p class="foot muted">New here, or lost your passkey? <a href="auth/enrol">Set up a passkey</a> with the enrolment link or recovery code you were given.</p>
  </Pane>
</main>

<style>
  .alone {
    max-width: 480px;
    margin: 18vh auto 0;
    padding: 0 16px;
  }

  p {
    margin: 0 0 calc(var(--unit) * 6);
    font-size: var(--type-navigation-size);
  }

  .note {
    color: var(--muted);
  }

  .status:empty {
    display: none;
  }


  details {
    margin: 0 0 calc(var(--unit) * 6);
  }

  summary {
    color: var(--accent);
    cursor: pointer;
    font-size: var(--type-navigation-size);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 2);
    margin-top: calc(var(--unit) * 4);
  }

  label {
    margin-top: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  input {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
  }

  form p {
    margin: calc(var(--unit) * 3) 0 0;
  }

  .foot {
    margin-bottom: 0;
    font-size: var(--type-control-size);
  }
</style>
