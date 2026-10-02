<script lang="ts" module>
  // What this browser offers for a passkey ceremony: why it can run none, where it cannot, and the
  // credentials container it runs one with.
  export type Passkeys = { unavailable: string; credentials?: CredentialsContainer };
</script>

<script lang="ts">
  import { Refusal, type API } from "../api/client";
  import Problem from "../components/Problem.svelte";
  import { Told, type Explained } from "../lib/problem";
  import type { Session } from "../lib/session.svelte";
  import { explainSignIn, PasswordsForbidden, sentence, signInWithPasskey, signInWithPassword } from "../lib/signin";

  // Signing in, where the API knows no session: with a passkey, the intended way, and with a password
  // as the fallback an installation may forbid. What the policy allows is read only once signed in,
  // so the password form is offered to anybody and withdrawn once the installation refuses a password
  // by naming the setting; hiding it is no policy, and the API refuses a password all the same.
  let { api, session, passkeys, version = "" }: { api: API; session: Session; passkeys: Passkeys; version?: string } = $props();

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

<main class="door">
  <div class="column">
    <div class="lockup">
      <svg viewBox="0 0 16 14" aria-hidden="true"
        ><rect x="0" y="0" width="16" height="4" rx="1" /><rect class="mid" x="0" y="6" width="7" height="4" rx="1" /><rect class="mid" x="9" y="6" width="7" height="4" rx="1" /><rect class="low" x="0" y="12" width="16" height="2" rx="1" /></svg
      >
      <span class="wordmark">agentiik</span>
    </div>
    <p class="headline">Workflow orchestration</p>

    <section class="card" aria-labelledby="sign-in">
      <h1 id="sign-in">Sign in</h1>
      {#if offline}
        <p class="note">{offline}</p>
      {:else}
        <button class="control primary wide" disabled={working} onclick={withPasskey}>Sign in with a passkey</button>
      {/if}

      {#if !forbidden}
        {#if !offline}<p class="or" aria-hidden="true"><span>or</span></p>{/if}
        <details bind:open={unfolded}>
          <summary class:quiet={offline}>{offline ? "With a password" : "Use a password instead"}</summary>
          <form onsubmit={withPassword}>
            <label for="login">Login</label>
            <input id="login" name="login" autocomplete="username" autocapitalize="none" spellcheck="false" required bind:value={login} />
            <label for="secret">Password</label>
            <input id="secret" name="password" type="password" autocomplete="current-password" required bind:value={password} />
            <label for="totp">One-time code</label>
            <input id="totp" name="totp" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" bind:value={totp} />
            <button class="control wide" class:primary={offline} type="submit" disabled={working}>Sign in with a password</button>
          </form>
        </details>
      {/if}

      <p class="status" role="status" aria-live="polite">{status}</p>
      {#if problem}<Problem explained={problem} />{/if}
    </section>

    <p class="foot"><a href="auth/enrol">Set up a passkey</a>{#if version}<span class="version">{version}</span>{/if}</p>
  </div>
</main>

<style>
  /* The door, after the site's opening: the graph canvas's dotted grid on the page's ground, a glow
     of the accent from the top fading into it, and the mark beside the name above the one thing the
     page is for. */
  .door {
    position: relative;
    display: grid;
    min-height: 100dvh;
    padding: calc(var(--unit) * 8);
    place-items: start center;
    isolation: isolate;
  }

  .door::before {
    content: "";
    position: absolute;
    inset: 0;
    z-index: -1;
    background:
      radial-gradient(70% 55% at 50% 0%, color-mix(in srgb, var(--accent) 20%, transparent), transparent 72%),
      radial-gradient(circle at 1px 1px, color-mix(in srgb, var(--faint) 45%, transparent) 1px, transparent 1.4px) 0 0 / 22px 22px;
    -webkit-mask-image: linear-gradient(to bottom, #000 55%, transparent);
    mask-image: linear-gradient(to bottom, #000 55%, transparent);
  }

  .column {
    display: grid;
    width: min(100%, 376px);
    margin-top: clamp(48px, 14vh, 152px);
    justify-items: center;
  }

  /* The lockup in the site's proportions: the mark as wide as 16 of its 14 units of height, the name
     15 of them high and set 9 of them away. */
  .lockup {
    --mark: 48px;
    display: inline-flex;
    align-items: center;
    gap: calc(var(--mark) * 9 / 14);
  }

  /* The mark's lighter bars are the accent mixed with the ground rather than made see-through, so
     that the dots do not show through them, as on the site. */
  .lockup svg {
    flex: none;
    width: calc(var(--mark) * 16 / 14);
    height: var(--mark);
    fill: var(--accent);
  }

  .lockup svg .mid {
    fill: color-mix(in srgb, var(--accent) 62%, var(--bg));
  }

  .lockup svg .low {
    fill: color-mix(in srgb, var(--accent) 30%, var(--bg));
  }

  .wordmark {
    font-family: var(--type-wordmark-font);
    font-size: calc(var(--mark) * 15 / 14);
    font-weight: 700;
    letter-spacing: -0.02em;
    line-height: var(--mark);
    text-transform: var(--type-wordmark-case);
  }

  .headline {
    margin: calc(var(--unit) * 8) 0 calc(var(--unit) * 18);
    color: var(--accent);
    font-family: var(--type-wordmark-font);
    font-size: 24px;
    font-weight: 700;
    letter-spacing: -0.03em;
    line-height: 32px;
  }

  .card {
    display: grid;
    width: 100%;
    gap: calc(var(--unit) * 4);
    padding: calc(var(--unit) * 12);
    border: var(--border-hairline) solid var(--line);
    border-radius: var(--radius-card);
    background: var(--surface);
    box-shadow: 0 12px 32px -12px color-mix(in srgb, var(--text) 18%, transparent);
  }

  h1 {
    margin: 0 0 calc(var(--unit) * 2);
    font-family: var(--type-pageTitle-font);
    font-size: var(--type-pageTitle-size);
    font-weight: var(--type-pageTitle-weight);
  }

  .wide {
    width: 100%;
    justify-content: center;
  }

  p {
    margin: 0;
    font-size: var(--type-navigation-size);
  }

  .note {
    color: var(--muted);
  }

  .status:empty {
    display: none;
  }

  /* or, between the two ways in, on a hairline. */
  .or {
    display: grid;
    grid-template-columns: 1fr auto 1fr;
    align-items: center;
    gap: calc(var(--unit) * 3);
    color: var(--faint);
    font-size: var(--type-control-size);
  }

  .or::before,
  .or::after {
    content: "";
    height: var(--border-hairline);
    background: var(--line);
  }

  summary {
    display: flex;
    align-items: center;
    justify-content: center;
    height: var(--control-height);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
    font-family: var(--type-control-font);
    font-size: var(--type-control-size);
    font-weight: var(--type-control-weight);
    cursor: pointer;
    list-style: none;
  }

  summary::-webkit-details-marker {
    display: none;
  }

  summary:hover {
    border-color: var(--faint);
  }

  details[open] summary {
    display: none;
  }

  /* With no passkey to offer, the password is the way in, unfolded under the reason. */
  summary.quiet {
    border-color: transparent;
    background: none;
    color: var(--accent);
  }

  form {
    display: grid;
    gap: calc(var(--unit) * 2);
  }

  label {
    margin-top: calc(var(--unit) * 2);
    font-size: var(--type-control-size);
  }

  label:first-child {
    margin-top: 0;
  }

  input {
    height: var(--control-height);
    padding: 0 calc(var(--unit) * 3);
    border: var(--border-hairline) solid var(--lineStrong);
    border-radius: var(--radius-control);
    background: var(--raised);
  }

  form .control {
    margin-top: calc(var(--unit) * 4);
  }

  .foot {
    display: flex;
    margin-top: calc(var(--unit) * 10);
    gap: calc(var(--unit) * 4);
    align-items: baseline;
    font-size: var(--type-control-size);
  }

  .version {
    color: var(--faint);
    font-family: var(--font-mono);
  }
</style>
