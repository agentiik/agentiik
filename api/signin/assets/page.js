// The sign-in and enrolment page: the two passkey ceremonies, the password fallback where the
// installation offers it, a password set from an enrolment code or from a session and a one-time
// code generator enrolled beside it, the credentials a signed-in account holds and their removal, a
// sign-in again where adding a way in asks for a recent one, who this browser is signed in as, and
// signing out. codec.js, loaded before it, converts what a browser without WebAuthn Level 3's JSON
// methods cannot, and qr.js draws a generator's key as a QR code.
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
  // directory, and answers its status, its JSON, null where it carries none, the seconds its
  // Retry-After gives, null where it gives none, and its WWW-Authenticate, null where it has none.
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
    return {
      status: response.status, answer, retryAfter: Number.isFinite(after) ? after : null,
      authenticate: response.headers.get("WWW-Authenticate"),
    };
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

  // forbidden says whether an answer refused a password because the policy that applies to the
  // account forbids passwords, which it says by naming the setting: the form it came from is not
  // offered to the account again.
  function forbidden(r) {
    return r.status === 403 && !!r.answer && r.answer.setting === "password";
  }

  // stale says whether an answer refused adding a way in for want of a recent sign-in, which it says
  // by the challenge RFC 9470 names rather than by its sentence: the page then asks for a sign-in
  // again.
  function stale(r) {
    return r.status === 403 && /\binsufficient_user_authentication\b/.test(r.authenticate || "");
  }

  // refusedAfter is an answer's sentence, and where it refused too many attempts, how long it asks
  // to wait.
  function refusedAfter(r) {
    return refusal(r) + (r.status === 429 ? wait(r.retryAfter) : "");
  }

  // drawQR draws text's QR code in container as SVG, made element by element and never written as
  // markup: dark modules on light whatever the page's colours, with the quiet zone of four modules a
  // scanner needs, the dark modules of each row joined into runs. Nothing is drawn for a text too
  // long for one, which the page shows as text all the same.
  function drawQR(container, text) {
    container.replaceChildren();
    let code;
    try {
      code = qr.encode(text);
    } catch (_) {
      return;
    }
    const ns = "http://www.w3.org/2000/svg";
    const n = code.modules.length;
    const side = String(n + 8);
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 " + side + " " + side);
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "The key as a QR code, for an authenticator application to scan");
    const ground = document.createElementNS(ns, "rect");
    ground.setAttribute("width", side);
    ground.setAttribute("height", side);
    ground.setAttribute("fill", "#fff");
    let d = "";
    code.modules.forEach((row, y) => {
      for (let x = 0; x < n; x++) {
        if (!row[x]) {
          continue;
        }
        let run = 1;
        while (x + run < n && row[x + run]) {
          run++;
        }
        d += "M" + (x + 4) + " " + (y + 4) + "h" + run + "v1h-" + run + "z";
        x += run - 1;
      }
    });
    const dark = document.createElementNS(ns, "path");
    dark.setAttribute("d", d);
    dark.setAttribute("fill", "#000");
    svg.appendChild(ground);
    svg.appendChild(dark);
    container.appendChild(svg);
  }

  // againSection is the sign-in asked for again, on both pages, where adding a way in, a first
  // password, a generator or a passkey registered from the session, found the session signed in to
  // longer ago than the API accepts: a passkey where a ceremony can run, and the password where the
  // account is offered one. A sign-in opens a session of its own, which the browser keeps in place of
  // the one before; done is what the page does once it has, and the person then tries again.
  function againSection(done) {
    const passkeys = !unavailable();
    let passwords = main.dataset.own === "offered";

    function over(r) {
      $("again-secret").value = "";
      $("again-totp").value = "";
      show("again", false);
      say("Signed in again as " + r.answer.login + ". Try once more.");
      return done();
    }

    $("again-passkey").addEventListener("click", busy($("again-passkey"), async () => {
      const started = await call("POST", "auth/passkey/options", { ceremony: "assertion" });
      if (started.status !== 200) {
        problem(refusal(started));
        return;
      }
      let credential;
      try {
        credential = await navigator.credentials.get({ publicKey: requestOptions(started.answer.options) });
      } catch (e) {
        problem(ceremonyProblem(e));
        return;
      }
      if (!credential) {
        problem("No passkey was offered. Try again, on this device or on a phone nearby.");
        return;
      }
      const verified = await call("POST", "auth/passkey/verify", { ceremony: "assertion", credential: credentialJSON(credential) });
      if (verified.status !== 200) {
        problem(refusal(verified));
        return;
      }
      await over(verified);
    }));

    $("again-password").addEventListener("submit", busy($("again-password-button"), async () => {
      const body = { login: $("again-login").value.trim(), password: $("again-secret").value };
      const totp = $("again-totp").value.trim();
      if (totp) {
        body.totp = totp;
      }
      $("again-secret").value = "";
      $("again-totp").value = "";
      const r = await call("POST", "auth/login", body);
      if (forbidden(r)) {
        passwords = false;
        show("again-password", false);
        problem(refusal(r));
        return;
      }
      if (r.status !== 200) {
        problem(refusedAfter(r));
        return;
      }
      await over(r);
    }));

    // ask shows the sign-in again, after the refusal that asked for it, and answers true where r is
    // that refusal.
    function ask(r) {
      if (!stale(r)) {
        return false;
      }
      say("");
      problem(refusal(r));
      show("again-passkey", passkeys);
      show("again-password", passwords);
      show("again", true);
      return true;
    }

    return { ask };
  }

  // day is the date an instant the API wrote falls on, as the API writes it, in UTC.
  function day(instant) {
    return typeof instant === "string" ? instant.slice(0, 10) : "";
  }

  // describe says what one credential is, for its holder: a passkey by its label and its kind, the
  // password and a generator by what they are, each with when it came and when it was last used.
  function describe(c) {
    const used = c.last_used_at ? ", last used " + day(c.last_used_at) : ", not used yet";
    switch (c.type) {
      case "passkey":
        return (c.label ? "“" + c.label + "”, a " : "A ") + c.kind + " passkey, enrolled " + day(c.created_at) + used;
      case "password":
        return "The password, set " + day(c.created_at) + used;
      case "totp":
        return "A one-time code generator, enrolled " + day(c.created_at) + used + ". It goes with the password, or with a code it shows below";
    }
    return "A credential of a kind this page does not know, enrolled " + day(c.created_at);
  }

  // credentialsSection is the list of what a signed-in account signs in with, on both pages, for a
  // full session, which GET /api/v1/me/credentials answers, each passkey and the password with the
  // button that removes it: the API refuses one the policy's minimum keeps, saying why, and the
  // generator is removed with a code it shows, in the section below. changed is what the page does
  // once the session may have changed: a credential removed ends the sessions it opened.
  function credentialsSection(changed) {
    async function render(login) {
      const list = $("credential-list");
      if (!login) {
        list.replaceChildren();
        show("credentials", false);
        return;
      }
      let r;
      try {
        r = await call("GET", "me/credentials");
      } catch (e) {
        r = { status: 0 };
      }
      if (r.status !== 200 || !r.answer || !Array.isArray(r.answer.credentials)) {
        list.replaceChildren();
        show("credentials", false);
        return;
      }
      list.replaceChildren();
      for (const c of r.answer.credentials) {
        const item = document.createElement("li");
        const what = document.createElement("span");
        what.textContent = describe(c);
        item.appendChild(what);
        if (c.type === "passkey" || c.type === "password") {
          const remove = document.createElement("button");
          remove.type = "button";
          remove.className = "quiet";
          remove.textContent = "Remove";
          remove.addEventListener("click", busy(remove, async () => {
            const removed = await call("DELETE", "me/credentials/" + encodeURIComponent(c.id));
            if (removed.status !== 204) {
              problem(refusal(removed));
              return;
            }
            say(c.type === "password" ? "The password is removed, with the one-time code generator beside it where there was one." : "The passkey is removed.");
            await changed();
          }));
          item.appendChild(remove);
        }
        list.appendChild(item);
      }
      show("credentials", true);
    }
    return { render };
  }

  // ownSection is the section a signed-in browser sets its password and its one-time code generator
  // in, on both pages, where passwords are offered to its account: the password set or changed, the
  // current one asked for where the account holds one, and removed; a generator started, its key
  // shown once as text and as a QR code, and turned on with a code it shows; and removed with one. A
  // session that may only enrol sets its password and nothing else. Once the API has refused this
  // account a password, the section is not offered again, and where adding a way in asks for a
  // recent sign-in, again asks for one. changed is what the page does once the session may have
  // changed: removing the password ends the sessions it opened.
  function ownSection(offered, changed, again) {
    let withdrawn = false;
    let login = null;

    function render(who) {
      login = who;
      show("own", offered && !withdrawn && login !== null && login !== undefined);
      show("own-more", !!login);
    }

    // withdraw takes the section away where the answer says passwords are forbidden to the
    // account, and answers whether it did.
    function withdraw(r) {
      if (!forbidden(r)) {
        return false;
      }
      withdrawn = true;
      say("");
      problem(refusal(r));
      render(login);
      return true;
    }

    // forget forgets a generator's key once it is turned on or given up: shown once, it is not
    // left on the page.
    function forget() {
      $("totp-secret").textContent = "";
      $("totp-uri").textContent = "";
      $("totp-qr").replaceChildren();
      show("totp-enrolling", false);
      show("totp-start", true);
    }

    $("change-password").addEventListener("submit", busy($("change-password-button"), async () => {
      const password = $("changed-password").value;
      const repeated = $("changed-password-again").value;
      const current = $("current-password").value;
      for (const id of ["changed-password", "changed-password-again", "current-password"]) {
        $(id).value = "";
      }
      if (password !== repeated) {
        problem("The two passwords differ. Type the same one in both fields.");
        return;
      }
      const body = { password };
      if (current) {
        body.current_password = current;
      }
      say("Setting your password…");
      const r = await call("PUT", "me/password", body);
      if (withdraw(r) || again.ask(r)) {
        return;
      }
      if (r.status !== 200) {
        say("");
        problem(refusedAfter(r));
        return;
      }
      say("Password set. Any other session it opened is signed out.");
    }));

    $("remove-password").addEventListener("click", busy($("remove-password"), async () => {
      const r = await call("DELETE", "me/password");
      if (r.status !== 204) {
        problem(refusal(r));
        return;
      }
      forget();
      await changed();
      say("Password removed, with the one-time code generator beside it where there was one.");
    }));

    $("totp-start").addEventListener("click", busy($("totp-start"), async () => {
      const r = await call("POST", "me/totp");
      if (withdraw(r) || again.ask(r)) {
        return;
      }
      if (r.status !== 200 || !r.answer || typeof r.answer.uri !== "string") {
        problem(refusal(r));
        return;
      }
      $("totp-secret").textContent = r.answer.secret;
      $("totp-uri").textContent = r.answer.uri;
      drawQR($("totp-qr"), r.answer.uri);
      show("totp-start", false);
      show("totp-enrolling", true);
      say("");
    }));

    $("totp-confirm").addEventListener("submit", busy($("totp-confirm-button"), async () => {
      const code = $("totp-code").value.trim();
      $("totp-code").value = "";
      const r = await call("POST", "me/totp/confirm", { totp: code });
      if (withdraw(r) || again.ask(r)) {
        return;
      }
      if (r.status !== 200) {
        problem(refusal(r));
        return;
      }
      forget();
      say("One-time codes are on: a sign-in with the password now asks for the code the application shows.");
    }));

    $("totp-remove").addEventListener("submit", busy($("totp-remove-button"), async () => {
      const code = $("totp-remove-code").value.trim();
      $("totp-remove-code").value = "";
      const r = await call("DELETE", "me/totp", { totp: code });
      if (r.status !== 204) {
        problem(refusedAfter(r));
        return;
      }
      say("The one-time code generator is removed.");
    }));

    return { render };
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
    // Whoever is signed in is offered the section setting their password, where passwords are
    // offered to their account, and a full session the list of what it signs in with.
    let settled = false;
    let withdrawn = false;
    const changed = async () => {
      const now = await signedIn();
      settled = true;
      render(now);
    };
    const again = againSection(changed);
    const credentials = credentialsSection(changed);
    const own = ownSection(main.dataset.own === "offered", changed, again);
    function render(login) {
      showSignedIn(login);
      own.render(login);
      credentials.render(login);
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
      if (forbidden(r)) {
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
    const passkeys = !why;
    const offered = main.dataset.password === "offered";
    if (why) {
      $("unavailable").textContent = why;
      show("unavailable", true);
    }
    if (linked && !enrolmentCode.test(linked)) {
      linked = "";
      problem("This link's code is incomplete. Open the whole link as it was given to you, or ask an administrator for a fresh one.");
    }

    // render shows what is left to do. A code, the link's or one typed in where no session is,
    // enrols a passkey where a ceremony can run and sets a password where the installation offers
    // them, in place of the passkey where none can run, until it is spent; a session adds a passkey
    // to its user, and sets their password in the section below. Once a code is spent, spent keeps
    // it from being asked for again; once the API has refused this account a password, withdrawn
    // keeps the form from coming back.
    let spent = false;
    let withdrawn = false;
    let current = null;
    const changed = async () => render(await signedIn());
    const again = againSection(changed);
    const credentials = credentialsSection(changed);
    const own = ownSection(main.dataset.own === "offered", changed, again);
    function render(login) {
      current = login;
      showSignedIn(login);
      own.render(login);
      credentials.render(login);
      const coded = !!linked || (login === null && !spent);
      const password = offered && !withdrawn && coded;
      show("code-field", coded && !linked && (passkeys || password));
      show("enrol", passkeys && (coded || login !== null));
      show("set-password", password);
      $("intro").textContent = intro(login, coded, password);
    }
    function intro(login, coded, password) {
      const or = password ? ", or set a password" : "";
      if (linked) {
        if (passkeys) {
          return "Your enrolment link is ready. Enrol a passkey, on this device or on a phone nearby, to sign in with from now on" + or + ".";
        }
        return password ? "Your enrolment link is ready. Set a password to sign in with from now on." : "";
      }
      if (login === "") {
        return "This browser is signed in to enrol a passkey, which the installation asks of your account before anything else.";
      }
      if (login) {
        return passkeys ? "Add a passkey to " + login + ", on this device or on a phone nearby." : "";
      }
      if (!coded) {
        return "";
      }
      if (passkeys) {
        return "Type the recovery code an administrator gave you to enrol a new passkey" + or + ". An enrolment link fills it in by itself.";
      }
      return password ? "Type the recovery code an administrator gave you to set a password. An enrolment link fills it in by itself." : "";
    }

    // codeAsked is the code a form enrols with, the link's or the one typed in, or "" where none
    // was, having said so where one outside its grammar was typed.
    function codeAsked() {
      const code = linked || $("code").value.trim();
      if (code && !enrolmentCode.test(code)) {
        problem("A code is agkenrol_ and at least 43 letters, digits, dashes and underscores after it, as it was given to you.");
        return null;
      }
      return code;
    }

    // codeSpent follows an enrolment that spent the code: the address stops carrying the link's,
    // and what comes next is done from the session the enrolment opened.
    function codeSpent() {
      if (linked) {
        linked = "";
        window.history.replaceState(null, "", window.location.pathname + window.location.search);
      }
      spent = true;
      $("code").value = "";
    }

    $("sign-out").addEventListener("click", busy($("sign-out"), async () => {
      if (await signOut()) {
        window.location.reload();
      }
    }));

    $("enrol").addEventListener("submit", busy($("enrol-button"), async () => {
      if (!passkeys) {
        // The form is not shown where no ceremony runs, and starts none if it is sent all the same.
        problem(why);
        return;
      }
      const asked = { ceremony: "registration" };
      const code = codeAsked();
      if (code === null) {
        return;
      }
      if (code) {
        asked.code = code;
      }
      const label = $("label").value.trim();
      say("Waiting for your authenticator…");
      const started = await call("POST", "auth/passkey/options", asked);
      if (again.ask(started)) {
        return;
      }
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
      if (again.ask(verified)) {
        return;
      }
      if (verified.status !== 200) {
        say("");
        problem(refusal(verified));
        return;
      }
      if (code) {
        codeSpent();
      }
      $("label").value = "";
      const passkey = verified.answer.credential || {};
      const named = passkey.label ? "“" + passkey.label + "”, " : "";
      $("enrolled-what").textContent = "Enrolled " + named + "a " + (passkey.kind || "new") + " passkey, for " + verified.answer.login + ".";
      show("enrolled", true);
      say("");
      const before = current;
      render(await signedIn());
      if (!code && before !== null && current === null) {
        // Registered from a session a password opened, which went with the password: the
        // passkeys held now are what the installation asks for, and the password was the way
        // to them.
        say("Your password is removed, now that you hold the passkeys the installation asks for, and this browser's session went with it: sign in with a passkey from now on.");
      }
      if (current !== null) {
        $("intro").textContent = "To enrol another, on another device or on a phone nearby:";
      }
      $("enrol-button").textContent = "Enrol another passkey";
    }));

    $("set-password").addEventListener("submit", busy($("set-password-button"), async () => {
      const password = $("new-password").value;
      const repeated = $("new-password-again").value;
      $("new-password").value = "";
      $("new-password-again").value = "";
      const code = codeAsked();
      if (code === null) {
        return;
      }
      if (!code) {
        problem("Setting a password takes the recovery code an administrator gave you, or the link it came in.");
        return;
      }
      if (password !== repeated) {
        problem("The two passwords differ. Type the same one in both fields.");
        return;
      }
      say("Setting your password…");
      const r = await call("POST", "auth/password/enrol", { code, password });
      if (forbidden(r)) {
        // Passwords are forbidden to this account, and the form is not offered to it again.
        withdrawn = true;
        say("");
        problem(refusal(r));
        render(current);
        return;
      }
      if (r.status !== 200) {
        say("");
        problem(refusal(r));
        return;
      }
      codeSpent();
      render(await signedIn());
      say("Password set for " + r.answer.login + "." + (r.answer.session === "enrolment"
        ? " The installation asks your account for a passkey before anything else: enrol one now."
        : ""));
    }));

    render(await signedIn());
  }

  (main.dataset.page === "enrol" ? enrolPage() : signInPage()).catch((e) => {
    problem(sentence(e && e.message ? e.message : e));
  });
})();
