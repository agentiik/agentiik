// The sign-in and enrolment page: the two passkey ceremonies, the password fallback where the
// installation offers it, who this browser is signed in as, and signing out. codec.js, loaded
// before it, converts what a browser without WebAuthn Level 3's JSON methods cannot.
//
// Every request is a fetch to the API on this page's own origin, with credentials same-origin, so
// that the session cookie travels with it and nowhere else, and in fetch's default mode, cors. The
// API refuses a request changing something whose Origin header is not the public URL's, and a
// browser writes Origin: null on a request of any other mode from a page under Referrer-Policy
// no-referrer, as it would on a form posted from here: so no form is posted and no request sets a
// mode. Every text the page shows is set as text, never as markup.
"use strict";

(() => {
  const main = document.getElementById("page");
  const $ = (id) => document.getElementById(id);

  // enrolmentCode is the grammar of an enrolment link's code and of a recovery code.
  const enrolmentCode = /^agkenrol_[A-Za-z0-9_-]{43,}$/;

  function show(id, on) {
    $(id).hidden = !on;
  }

  function say(text) {
    $("status").textContent = text;
  }

  function problem(text) {
    $("problem").textContent = text;
    $("problem").hidden = !text;
  }

  // sentence is one of the API's sentences as the page shows it, with a capital and a full stop.
  function sentence(text) {
    const s = String(text);
    return s.charAt(0).toUpperCase() + s.slice(1) + (/[.!?]$/.test(s) ? "" : ".");
  }

  // call sends one request to the API, whose routes are under api/v1 beside the page's own
  // directory, and answers its status, its JSON, null where it carries none, and the seconds its
  // Retry-After gives, null where it gives none.
  async function call(method, path, body) {
    const init = { method, credentials: "same-origin", cache: "no-store", headers: {} };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    let response;
    try {
      response = await fetch(new URL("../api/v1/" + path, document.baseURI), init);
    } catch (e) {
      throw new Error("the installation could not be reached: " + (e && e.message ? e.message : e));
    }
    let answer = null;
    try {
      answer = await response.json();
    } catch (_) {
      answer = null;
    }
    const after = Number.parseInt(response.headers.get("Retry-After"), 10);
    return { status: response.status, answer, retryAfter: Number.isFinite(after) ? after : null };
  }

  // wait says, for a person, how long Retry-After asks them to wait, in whole minutes rounded up,
  // or seconds under a minute: the API's sentence speaks of the header, which a person never sees.
  function wait(seconds) {
    if (seconds === null || seconds <= 0) {
      return "";
    }
    if (seconds < 60) {
      return " Try again in " + seconds + (seconds === 1 ? " second." : " seconds.");
    }
    const minutes = Math.ceil(seconds / 60);
    return " Try again in " + minutes + (minutes === 1 ? " minute." : " minutes.");
  }

  // refusal is the sentence an answer refused with, or one of the page's where it carries none.
  function refusal(r) {
    if (r.answer && typeof r.answer.error === "string") {
      return sentence(r.answer.error);
    }
    return "The installation answered " + r.status + " and said nothing more.";
  }

  // ceremonyProblem says what a browser's refusal of a ceremony means to the person at it.
  function ceremonyProblem(e) {
    switch (e && e.name) {
      case "NotAllowedError":
      case "AbortError":
        return "The passkey ceremony was cancelled, or it timed out. Try again when you are ready.";
      case "InvalidStateError":
        return "This authenticator holds a passkey for this account already. Enrol the next one on another device, or on a phone nearby.";
      case "SecurityError":
        return "The browser refused to run a passkey ceremony on this page. It runs one only on a page served over https with a certificate it trusts, never one clicked past a warning, at the address the installation is known by.";
      case "NotSupportedError":
        return "This authenticator makes no passkey of a kind the installation accepts: ES256, EdDSA or RS256.";
    }
    return "The passkey ceremony failed: " + (e && e.message ? e.message : String(e));
  }

  // unavailable says why no passkey ceremony can run on this page, and nothing where one can.
  function unavailable() {
    if (main.dataset.passkeys === "unavailable") {
      return "Passkeys are unavailable on this installation: it is addressed by an IP address, and a browser runs a passkey ceremony only for a name. Ask its administrator to address it by one.";
    }
    if (!window.isSecureContext || !window.PublicKeyCredential || !navigator.credentials) {
      return "This browser offers no passkeys on this page. A browser runs a passkey ceremony only on a page served over https with a certificate it trusts, never one clicked past a warning.";
    }
    return "";
  }

  // The JSON forms, the browser's own where it has them.
  function creationOptions(json) {
    if (typeof PublicKeyCredential.parseCreationOptionsFromJSON === "function") {
      return PublicKeyCredential.parseCreationOptionsFromJSON(json);
    }
    return codec.creationOptions(json);
  }

  function requestOptions(json) {
    if (typeof PublicKeyCredential.parseRequestOptionsFromJSON === "function") {
      return PublicKeyCredential.parseRequestOptionsFromJSON(json);
    }
    return codec.requestOptions(json);
  }

  // credentialJSON is the browser's toJSON(), or codec's where it has none or it throws, as it does
  // on the stand-in some password managers answer with.
  function credentialJSON(credential) {
    if (typeof credential.toJSON === "function") {
      try {
        return credential.toJSON();
      } catch (_) {
        // codec's, below.
      }
    }
    return codec.credentialJSON(credential);
  }

  // signedIn is who this browser is signed in as: the login of a full session, "" for a session
  // that may only enrol, which GET /api/v1/me refuses with 403, and null for none.
  async function signedIn() {
    let r;
    try {
      r = await call("GET", "me");
    } catch (_) {
      return null;
    }
    if (r.status === 200 && r.answer && typeof r.answer.principal === "string") {
      return r.answer.principal;
    }
    return r.status === 403 ? "" : null;
  }

  // showSignedIn says who this browser is signed in as, as signedIn answers it, with the button
  // that signs it out: a session that may only enrol is one to sign out of too, on a shared machine
  // above all, since whoever comes next could enrol a passkey of their own on the account.
  function showSignedIn(login) {
    const signed = login !== null && login !== undefined;
    $("who").textContent = login ? "Signed in as " + login + "." : signed ? "Signed in to enrol a passkey, and nothing else." : "";
    show("signed-in", signed);
  }

  // busy runs work for a button, which stays disabled until it is done, and shows what went wrong.
  function busy(button, work) {
    return async (event) => {
      if (event) {
        event.preventDefault();
      }
      if (button.disabled) {
        return;
      }
      button.disabled = true;
      problem("");
      try {
        await work();
      } catch (e) {
        say("");
        problem(sentence(e && e.message ? e.message : e));
      } finally {
        button.disabled = false;
      }
    };
  }

  // signOut ends this browser's session, and answers whether it did.
  async function signOut() {
    const r = await call("POST", "auth/sign-out");
    if (r.status !== 204) {
      problem(refusal(r));
      return false;
    }
    showSignedIn(null);
    say("Signed out.");
    return true;
  }

  // The sign-in page.
  async function signInPage() {
    const redirect = main.dataset.terminalRedirect;
    const challenge = main.dataset.terminalChallenge;
    const handOff = redirect && challenge ? { redirect_uri: redirect, code_challenge: challenge } : null;
    const why = unavailable();
    const offered = main.dataset.password === "offered";

    // render shows what is left to do: signing in, where nobody is or where agk login waits for a
    // sign-in of its own, and otherwise who is signed in. A session that may only enrol is shown the
    // way to enrol a passkey and its sign-out, and no sign-in: signing in again, with the password
    // that opened it, opens another such session, and with a passkey is what enrolling one is for,
    // agk login's included, since such a session mints no token. Once a sign-in or a sign-out on the
    // page has said who that is, settled keeps the page's first question from answering over it;
    // once the API has refused this account a password, withdrawn keeps the form from coming back.
    let settled = false;
    let withdrawn = false;
    function render(login) {
      showSignedIn(login);
      show("enrolling", login === "");
      const signing = login !== "" && (!login || !!handOff);
      show("passkey", signing && !why);
      show("password", signing && offered && !withdrawn);
    }

    // signedInAs follows a sign-in: back to agk login where it opened the page, at the address the
    // API wrote once it has checked it is agk login's, and otherwise to who is signed in.
    function signedInAs(answer) {
      if (handOff && typeof answer.redirect_to === "string") {
        if (!answer.redirect_to.startsWith(handOff.redirect_uri + "?code=")) {
          say("");
          problem("The installation answered an address other than the one agk login listens on, and this page does not follow it.");
          return;
        }
        say("Signed in as " + answer.login + ". Handing agk login its code: close this page once the terminal says it is signed in.");
        window.location.assign(answer.redirect_to);
        return;
      }
      say("");
      settled = true;
      render(answer.login);
    }

    $("sign-in").addEventListener("click", busy($("sign-in"), async () => {
      say("Waiting for your passkey…");
      const started = await call("POST", "auth/passkey/options", { ceremony: "assertion" });
      if (started.status !== 200) {
        say("");
        problem(refusal(started));
        return;
      }
      let credential;
      try {
        credential = await navigator.credentials.get({ publicKey: requestOptions(started.answer.options) });
      } catch (e) {
        say("");
        problem(ceremonyProblem(e));
        return;
      }
      if (!credential) {
        say("");
        problem("No passkey was offered. Try again, on this device or on a phone nearby.");
        return;
      }
      const body = { ceremony: "assertion", credential: credentialJSON(credential) };
      if (handOff) {
        body.terminal = handOff;
      }
      const verified = await call("POST", "auth/passkey/verify", body);
      if (verified.status !== 200) {
        say("");
        problem(refusal(verified));
        return;
      }
      signedInAs(verified.answer);
    }));

    $("password").addEventListener("submit", busy($("password-button"), async () => {
      const body = { login: $("login").value.trim(), password: $("secret").value };
      const totp = $("totp").value.trim();
      if (totp) {
        body.totp = totp;
      }
      if (handOff) {
        body.terminal = handOff;
      }
      $("secret").value = "";
      $("totp").value = "";
      const r = await call("POST", "auth/login", body);
      if (r.status === 403 && r.answer && r.answer.setting === "password") {
        // Passwords are forbidden to this account, and the form is not offered to it again.
        withdrawn = true;
        show("password", false);
        problem(refusal(r));
        return;
      }
      if (r.status === 429) {
        problem(refusal(r) + wait(r.retryAfter));
        return;
      }
      if (r.status !== 200) {
        problem(refusal(r));
        return;
      }
      if (r.answer.session === "enrolment") {
        settled = true;
        render("");
        return;
      }
      signedInAs(r.answer);
    }));

    $("sign-out").addEventListener("click", busy($("sign-out"), async () => {
      if (await signOut()) {
        settled = true;
        render(null);
      }
    }));

    show("terminal", !!handOff);
    if (why) {
      $("unavailable").textContent = why;
      show("unavailable", true);
    }
    // Signed out until the API says otherwise, which is what most who open this page are, so that
    // the page is not empty while it asks.
    render(null);
    const login = await signedIn();
    if (!settled) {
      render(login);
    }
  }

  // The enrolment page.
  async function enrolPage() {
    // The code of an enrolment link, after the # of this page's address, which a browser never
    // sends, so that it reaches no access log; or none, where a recovery code is typed instead or
    // the browser's session enrols.
    let linked = window.location.hash.slice(1);
    const why = unavailable();
    if (why) {
      $("unavailable").textContent = why;
      show("unavailable", true);
    }
    const login = await signedIn();
    showSignedIn(login);
    $("sign-out").addEventListener("click", busy($("sign-out"), async () => {
      if (await signOut()) {
        window.location.reload();
      }
    }));
    if (why) {
      return;
    }
    if (linked && !enrolmentCode.test(linked)) {
      linked = "";
      problem("This link's code is incomplete. Open the whole link as it was given to you, or ask an administrator for a fresh one.");
    }
    if (linked) {
      $("intro").textContent = "Your enrolment link is ready. Enrol a passkey, on this device or on a phone nearby, to sign in with from now on.";
    } else if (login) {
      $("intro").textContent = "Add a passkey to " + login + ", on this device or on a phone nearby.";
    } else if (login === "") {
      $("intro").textContent = "This browser is signed in to enrol a passkey, which the installation asks of your account before anything else.";
    } else {
      $("intro").textContent = "Type the recovery code an administrator gave you to enrol a new passkey. An enrolment link fills it in by itself.";
    }
    show("code-field", !linked && login === null);
    show("enrol", true);

    $("enrol").addEventListener("submit", busy($("enrol-button"), async () => {
      const asked = { ceremony: "registration" };
      const code = linked || $("code").value.trim();
      if (code) {
        if (!enrolmentCode.test(code)) {
          problem("A code is agkenrol_ and at least 43 letters, digits, dashes and underscores after it, as it was given to you.");
          return;
        }
        asked.code = code;
      }
      const label = $("label").value.trim();
      say("Waiting for your authenticator…");
      const started = await call("POST", "auth/passkey/options", asked);
      if (started.status !== 200) {
        say("");
        problem(refusal(started));
        return;
      }
      let credential;
      try {
        credential = await navigator.credentials.create({ publicKey: creationOptions(started.answer.options) });
      } catch (e) {
        say("");
        problem(ceremonyProblem(e));
        return;
      }
      if (!credential) {
        say("");
        problem("No passkey was made. Try again, on this device or on a phone nearby.");
        return;
      }
      const body = { ceremony: "registration", credential: credentialJSON(credential) };
      if (label) {
        body.label = label;
      }
      const verified = await call("POST", "auth/passkey/verify", body);
      if (verified.status !== 200) {
        say("");
        problem(refusal(verified));
        return;
      }
      if (linked) {
        // The code is spent: the address stops carrying it, and a next passkey is added from the
        // session the enrolment opened.
        linked = "";
        window.history.replaceState(null, "", window.location.pathname + window.location.search);
      }
      $("code").value = "";
      $("label").value = "";
      const passkey = verified.answer.credential || {};
      const named = passkey.label ? "“" + passkey.label + "”, " : "";
      $("enrolled-what").textContent = "Enrolled " + named + "a " + (passkey.kind || "new") + " passkey, for " + verified.answer.login + ".";
      show("enrolled", true);
      say("");
      const now = await signedIn();
      showSignedIn(now);
      show("code-field", false);
      $("intro").textContent = now === null ? "" : "To enrol another, on another device or on a phone nearby:";
      $("enrol-button").textContent = "Enrol another passkey";
      show("enrol", now !== null);
    }));
  }

  (main.dataset.page === "enrol" ? enrolPage() : signInPage()).catch((e) => {
    problem(sentence(e && e.message ? e.message : e));
  });
})();
