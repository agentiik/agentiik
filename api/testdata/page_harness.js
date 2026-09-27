// Run by TestThePageScriptSignsInEnrolsAndSignsOutOnAStandInBrowser, after codec.js, page.js wrapped
// in loadPage(), which runs it as a browser does on each page load, and the pages the test wrote:
// each element of a page by its id, hidden or not as the HTML says, and the page's data attributes.
// It stands in for the browser page.js reads, the DOM, fetch, navigator.credentials, location and
// history, drives each scenario a person would, answering each request as the API would, and
// prints, in one line of JSON, what went otherwise than a scenario says. It runs on node and on
// macOS's jsc, which prints with print and has no console and no URL.
const say = typeof print === "function" ? print : (line) => console.log(line);

// A URL enough for page.js's own, "../api/v1/..." against the page's address, where there is none.
const StandInURL = class {
  constructor(relative, base) {
    let dir = base.replace(/[?#].*$/, "").replace(/[^/]*$/, "");
    let rest = relative;
    while (rest.startsWith("../")) {
      rest = rest.slice(3);
      dir = dir.replace(/[^/]*\/$/, "");
    }
    this.href = dir + rest;
  }
  toString() {
    return this.href;
  }
};

function global(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

// stage loads a page, as the test wrote it, at an address, and answers its browser: one whose
// address carries hash after its #, which is not a secure context where insecure is set, and whose
// authenticator's credentials have no toJSON() where raw is set.
function stage(name, address, { hash, insecure, raw } = {}) {
  const written = pages[name];
  const elements = {};
  for (const id of Object.keys(written.elements)) {
    elements[id] = {
      id, hidden: written.elements[id].hidden, textContent: "", value: "", disabled: false, dataset: {}, listeners: {},
      addEventListener(type, f) {
        (this.listeners[type] = this.listeners[type] || []).push(f);
      },
    };
  }
  for (const [attribute, value] of Object.entries(written.data)) {
    elements.page.dataset[attribute.replace(/^data-/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = value;
  }
  const browser = { elements, requests: [], created: [], got: [], location: null };
  // Where the page's requests go: api/v1 beside the page's own directory, under the public URL's
  // path where it has one.
  browser.api = address.replace(/[?#].*$/, "").replace(/\/auth\/[^/]*$/, "/api/v1/");
  const path = address.replace(/^https:\/\/[^/]+/, "");
  browser.location = {
    hash: hash || "", pathname: path.replace(/\?.*$/, ""), search: path.includes("?") ? path.slice(path.indexOf("?")) : "",
    assigned: null, replaced: null, reloaded: false,
    assign(to) {
      this.assigned = String(to);
    },
    reload() {
      this.reloaded = true;
    },
  };
  const PublicKeyCredential = function () {};
  global("document", { getElementById: (id) => elements[id] || null, baseURI: address });
  global("window", {
    isSecureContext: !insecure, PublicKeyCredential, location: browser.location,
    history: { replaceState: (_, __, to) => { browser.location.replaced = to; } },
  });
  global("PublicKeyCredential", PublicKeyCredential);
  global("navigator", {
    credentials: {
      get: async (options) => {
        browser.got.push(options);
        if (raw) {
          const bytes = (...b) => new Uint8Array(b).buffer;
          return {
            id: "AQID", rawId: bytes(1, 2, 3), type: "public-key", authenticatorAttachment: "platform",
            response: { clientDataJSON: bytes(4), authenticatorData: bytes(5, 6), signature: bytes(7), userHandle: bytes(8, 9, 10, 11) },
            getClientExtensionResults: () => ({}),
          };
        }
        return { toJSON: () => ({ id: "cred", rawId: "cred", type: "public-key", response: {} }) };
      },
      create: async (options) => {
        browser.created.push(options);
        return { toJSON: () => ({ id: "made", rawId: "made", type: "public-key", response: {} }) };
      },
    },
  });
  global("fetch", (url, init) => new Promise((resolve) => {
    browser.requests.push({
      url: String(url), init, body: init && init.body ? JSON.parse(init.body) : undefined, answered: false,
      answer(status, json, headers = {}) {
        this.answered = true;
        resolve({
          status,
          headers: { get: (name) => Object.entries(headers).find(([k]) => k.toLowerCase() === name.toLowerCase())?.[1] ?? null },
          json: async () => { if (json === undefined) { throw new SyntaxError("no body"); } return json; },
        });
      },
    });
  }));
  if (typeof URL === "undefined") {
    global("URL", StandInURL);
  }
  loadPage();
  return browser;
}

// settle lets every promise that can go on go on.
async function settle() {
  for (let i = 0; i < 200; i++) {
    await null;
  }
}

const failures = [];
let scenario = "";
function check(ok, what) {
  if (!ok) {
    failures.push(scenario + ": " + what);
  }
}

// request is the first request to the API route not yet answered, or a failure where there is none.
function request(browser, route) {
  const r = browser.requests.find((q) => !q.answered && q.url.endsWith("/api/v1/" + route));
  check(!!r, "no request to " + route + " was sent; sent: " + browser.requests.map((q) => q.url).join(", "));
  return r || { answer() {}, body: {}, init: {} };
}

// fire dispatches an event to an element, as a click or a submit does, and lets the page go on as
// far as it can without an answer it is waiting for.
async function fire(browser, id, type) {
  for (const f of browser.elements[id].listeners[type] || []) {
    f({ preventDefault() {} });
  }
  await settle();
}

const visible = (browser, id) => !browser.elements[id].hidden;
const text = (browser, id) => browser.elements[id].textContent;
const assertion = { ceremony: "assertion", options: { challenge: "AAAA", timeout: 300000, rpId: "agentiik.example.com", allowCredentials: [], userVerification: "required" } };
const registration = {
  ceremony: "registration",
  options: {
    rp: { id: "agentiik.example.com", name: "Agentiik" }, user: { id: "AAAA", name: "alice", displayName: "Alice" }, challenge: "AAAA",
    pubKeyCredParams: [{ type: "public-key", alg: -7 }], timeout: 300000, excludeCredentials: [],
    authenticatorSelection: { residentKey: "required", requireResidentKey: true, userVerification: "required" }, attestation: "none",
  },
};
const code = "agkenrol_oQtE7pKEuHkJTWZiELmHaW4Bc1fH6P_xBs51HPJ6f9U";
const all = [];

async function scenarios() {
  if (answers) {
    await passwordAnswers();
    return;
  }
  let b;

  scenario = "signed out";
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  check(visible(b, "passkey"), "the passkey button is not shown while the page asks who is signed in");
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "passkey") && !visible(b, "password") && !visible(b, "signed-in") && !visible(b, "enrolling") && !visible(b, "terminal"),
    "the page shows " + Object.keys(b.elements).filter((id) => visible(b, id)).join(", "));

  scenario = "a session that may only enrol, on the sign-in page";
  b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(visible(b, "enrolling"), "the page does not say the account needs a passkey first");
  check(!visible(b, "passkey") && !visible(b, "password"), "the page offers a sign-in to a session that may only enrol");
  check(visible(b, "signed-in") && /enrol/.test(text(b, "who")), "the page offers no sign-out: " + JSON.stringify(text(b, "who")));
  await fire(b, "sign-out", "click");
  const out = request(b, "auth/sign-out");
  check(out.init.method === "POST", "the sign-out is sent as " + out.init.method);
  out.answer(204);
  await settle();
  check(!visible(b, "signed-in") && !visible(b, "enrolling") && text(b, "status") === "Signed out.", "the page did not sign out: " + text(b, "status"));
  check(visible(b, "passkey") && visible(b, "password"), "the page offers no sign-in once signed out");

  scenario = "a session that may only enrol, on a sign-in agk login opened";
  b = stage("sign-in-password-terminal", "https://agentiik.example.com/auth/sign-in?redirect_uri=x");
  all.push(b);
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(visible(b, "terminal") && visible(b, "enrolling") && visible(b, "signed-in"), "the page does not send the session to enrol a passkey");
  check(!visible(b, "passkey") && !visible(b, "password"), "the page offers agk login a sign-in from a session that may only enrol");

  scenario = "passwords withdrawn before the page knows who is signed in";
  b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  check(visible(b, "password"), "the password form is not offered");
  b.elements.login.value = " alice ";
  b.elements.secret.value = "correct horse battery staple";
  await fire(b, "password", "submit");
  const login = request(b, "auth/login");
  check(login.body.login === "alice" && login.body.password === "correct horse battery staple" && !("terminal" in login.body),
    "the password sign-in sent " + JSON.stringify(login.body));
  check(b.elements.secret.value === "", "the password is left in its field");
  login.answer(403, { error: "passwords are forbidden by the policy that applies to this account", setting: "password" });
  await settle();
  check(!visible(b, "password") && visible(b, "problem"), "the password form is still offered once the API withdrew it");
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(!visible(b, "password"), "the password form came back once the page learned nobody is signed in");

  scenario = "a sign-in before the page knows who is signed in";
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  await fire(b, "sign-in", "click");
  request(b, "auth/passkey/options").answer(200, assertion);
  await settle();
  request(b, "auth/passkey/verify").answer(200, { ceremony: "assertion", login: "alice" });
  await settle();
  check(visible(b, "signed-in") && text(b, "who") === "Signed in as alice." && !visible(b, "passkey"), "the page does not say alice signed in");
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "signed-in") && !visible(b, "passkey"), "the page's first question answered over the sign-in");

  for (const [where, back, followed] of [
    ["agk login's address", "http://127.0.0.1:53682/callback?code=agkcode_hETdtl86N8K51a1xaF2Tykj9jlYJfsaq-DTMlDPeDZ0", true],
    ["another address", "http://127.0.0.1:53682/callbackx?code=agkcode_hETdtl86N8K51a1xaF2Tykj9jlYJfsaq-DTMlDPeDZ0", false],
    ["another host", "https://evil.example/?code=agkcode_hETdtl86N8K51a1xaF2Tykj9jlYJfsaq-DTMlDPeDZ0", false],
  ]) {
    scenario = "a passkey sign-in agk login opened, answered with " + where;
    b = stage("sign-in-terminal", "https://agentiik.example.com/auth/sign-in?redirect_uri=x");
    all.push(b);
    await settle();
    request(b, "me").answer(401, { error: "this request carries no credential" });
    await settle();
    check(visible(b, "terminal") && visible(b, "passkey"), "the page does not say agk login waits");
    await fire(b, "sign-in", "click");
    const options = request(b, "auth/passkey/options");
    check(JSON.stringify(options.body) === JSON.stringify({ ceremony: "assertion" }), "the options asked were " + JSON.stringify(options.body));
    options.answer(200, assertion);
    await settle();
    check(b.got.length === 1 && b.got[0].publicKey && b.got[0].publicKey.challenge instanceof ArrayBuffer, "navigator.credentials.get was not handed the options");
    const verify = request(b, "auth/passkey/verify");
    check(verify.body.ceremony === "assertion" && verify.body.credential.id === "cred" &&
      JSON.stringify(verify.body.terminal) === JSON.stringify({ redirect_uri: pages["sign-in-terminal"].data["data-terminal-redirect"], code_challenge: pages["sign-in-terminal"].data["data-terminal-challenge"] }),
      "the verification sent " + JSON.stringify(verify.body));
    verify.answer(200, { ceremony: "assertion", login: "alice", redirect_to: back });
    await settle();
    check((b.location.assigned === back) === followed, "the page went to " + b.location.assigned);
    check(followed || visible(b, "problem"), "the page says nothing of an address it does not follow");
  }

  scenario = "an enrolment link";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "enrol") && !visible(b, "code-field") && /link/.test(text(b, "intro")), "the page does not offer to enrol from the link");
  b.elements.label.value = " work laptop ";
  await fire(b, "enrol", "submit");
  const asked = request(b, "auth/passkey/options");
  check(JSON.stringify(asked.body) === JSON.stringify({ ceremony: "registration", code }), "the options asked were " + JSON.stringify(asked.body));
  check(!asked.url.includes(code), "the code travelled in an address: " + asked.url);
  asked.answer(200, registration);
  await settle();
  check(b.created.length === 1 && b.created[0].publicKey.user.id instanceof ArrayBuffer, "navigator.credentials.create was not handed the options");
  const made = request(b, "auth/passkey/verify");
  check(made.body.ceremony === "registration" && made.body.label === "work laptop" && made.body.credential.id === "made" && !("code" in made.body),
    "the verification sent " + JSON.stringify(made.body));
  made.answer(200, { ceremony: "registration", login: "alice", credential: { type: "passkey", id: "made", label: "work laptop", kind: "device-bound" } });
  await settle();
  check(b.location.replaced === "/auth/enrol", "the spent code is left in the address: " + b.location.replaced);
  check(visible(b, "enrolled") && /work laptop/.test(text(b, "enrolled-what")) && /device-bound/.test(text(b, "enrolled-what")), "the page does not say what was enrolled");
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  check(visible(b, "signed-in") && text(b, "who") === "Signed in as alice." && visible(b, "enrol"), "the page does not offer another passkey to alice");

  scenario = "a session that may only enrol, on the enrolment page";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol");
  all.push(b);
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(visible(b, "enrol") && !visible(b, "code-field"), "the page does not enrol from the session");
  check(visible(b, "signed-in"), "the page offers no sign-out");

  scenario = "a recovery code";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol");
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "code-field"), "the page asks for no recovery code");
  b.elements.code.value = "agkenrol_short";
  await fire(b, "enrol", "submit");
  check(visible(b, "problem") && !b.requests.some((q) => q.url.endsWith("auth/passkey/options")), "a code outside its grammar was sent");

  scenario = "an installation addressed by an IP address, on the sign-in page";
  b = stage("sign-in-ip", "https://192.0.2.10/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "unavailable") && /IP address/.test(text(b, "unavailable")), "the page does not say why passkeys are unavailable: " + JSON.stringify(text(b, "unavailable")));
  check(!visible(b, "passkey") && visible(b, "password"), "the page offers a passkey, or no password");

  scenario = "an installation addressed by an IP address, on the enrolment page";
  b = stage("enrol-ip", "https://192.0.2.10/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  await fire(b, "enrol", "submit");
  check(visible(b, "unavailable") && /IP address/.test(text(b, "unavailable")), "the page does not say why passkeys are unavailable");
  check(!visible(b, "enrol") && !b.requests.some((q) => q.url.endsWith("auth/passkey/options")), "the page offers to enrol a passkey");

  scenario = "a page served where no passkey runs";
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in", { insecure: true });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "unavailable") && /https/.test(text(b, "unavailable")) && !visible(b, "passkey"), "the page offers a passkey where the browser runs none");

  scenario = "a browser whose credentials have no toJSON()";
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in", { raw: true });
  all.push(b);
  await settle();
  await fire(b, "sign-in", "click");
  request(b, "auth/passkey/options").answer(200, assertion);
  await settle();
  const converted = request(b, "auth/passkey/verify").body.credential;
  check(JSON.stringify(converted) === JSON.stringify({
    id: "AQID", rawId: "AQID", type: "public-key",
    response: { clientDataJSON: "BA", authenticatorData: "BQY", signature: "Bw", userHandle: "CAkKCw" },
    authenticatorAttachment: "platform", clientExtensionResults: {},
  }), "the page wrote the credential " + JSON.stringify(converted));

  scenario = "a page under the public URL's path";
  b = stage("sign-in", "https://agentiik.example.com/agentiik/auth/sign-in");
  all.push(b);
  await settle();
  check(b.requests.length === 1 && b.requests[0].url === "https://agentiik.example.com/agentiik/api/v1/me", "the page asked " + b.requests.map((q) => q.url).join(", "));

  for (const [what, answer, name] of [
    ["a full session agk login opened", { login: "alice", session: "full", redirect_to: "http://127.0.0.1:53682/callback?code=agkcode_hETdtl86N8K51a1xaF2Tykj9jlYJfsaq-DTMlDPeDZ0" }, "sign-in-password-terminal"],
    ["a full session", { login: "alice", session: "full" }, "sign-in-password"],
    ["a session that may only enrol", { login: "bob-martin", session: "enrolment" }, "sign-in-password"],
  ]) {
    scenario = "a password sign-in answered with " + what;
    b = stage(name, "https://agentiik.example.com/auth/sign-in");
    all.push(b);
    await settle();
    request(b, "me").answer(401, { error: "this request carries no credential" });
    await settle();
    b.elements.login.value = answer.login;
    b.elements.secret.value = "correct horse battery staple";
    b.elements.totp.value = "492039";
    await fire(b, "password", "submit");
    const sent = request(b, "auth/login");
    const handedOff = name.endsWith("terminal") ? { redirect_uri: pages[name].data["data-terminal-redirect"], code_challenge: pages[name].data["data-terminal-challenge"] } : undefined;
    check(JSON.stringify(sent.body) === JSON.stringify({ login: answer.login, password: "correct horse battery staple", totp: "492039", terminal: handedOff }),
      "the password sign-in sent " + JSON.stringify(sent.body));
    sent.answer(200, answer);
    await settle();
    if (answer.redirect_to) {
      check(b.location.assigned === answer.redirect_to, "the page went to " + b.location.assigned);
    } else if (answer.session === "enrolment") {
      check(visible(b, "enrolling") && visible(b, "signed-in") && /enrol/.test(text(b, "who")), "the page does not send bob-martin to enrol a passkey");
      check(!visible(b, "password") && !visible(b, "passkey"), "the page offers bob-martin another sign-in");
    } else {
      check(visible(b, "signed-in") && text(b, "who") === "Signed in as alice." && !visible(b, "password") && !visible(b, "passkey"), "the page does not say alice signed in");
    }
  }

  scenario = "an incomplete enrolment link";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol", { hash: "#agkenrol_oQtE7pKE" });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "problem") && /incomplete/.test(text(b, "problem")) && visible(b, "code-field"), "the page does not say the link is incomplete");

  scenario = "an enrolment that opens no session";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  await fire(b, "enrol", "submit");
  request(b, "auth/passkey/options").answer(200, registration);
  await settle();
  request(b, "auth/passkey/verify").answer(200, { ceremony: "registration", login: "dave", credential: { type: "passkey", id: "made", kind: "synced" } });
  await settle();
  request(b, "me").answer(401, { error: "that session opens nothing" });
  await settle();
  check(visible(b, "enrolled") && /synced/.test(text(b, "enrolled-what")), "the page does not say what was enrolled");
  check(!visible(b, "enrol") && !visible(b, "signed-in"), "the page offers another passkey with no session to add it from");

  scenario = "a sign-out on the enrolment page";
  b = stage("enrol", "https://agentiik.example.com/auth/enrol");
  all.push(b);
  await settle();
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  check(visible(b, "signed-in") && /alice/.test(text(b, "intro")) && !visible(b, "code-field"), "the page does not add a passkey to alice");
  await fire(b, "sign-out", "click");
  request(b, "auth/sign-out").answer(204);
  await settle();
  check(b.location.reloaded, "the page is not loaded again once signed out");

  scenario = "every request";
  for (const browser of all) {
    for (const q of browser.requests) {
      check(q.url.startsWith(browser.api), "a request went to " + q.url + " rather than under " + browser.api);
      check(q.init.credentials === "same-origin" && q.init.mode === undefined, "a request to " + q.url + " was sent with credentials " + q.init.credentials + " and mode " + q.init.mode);
    }
  }
}

// passwordAnswers drives the password form against what POST /api/v1/auth/login answered the test,
// over a real PostgreSQL, each answer as the route wrote it, its status, its body and its
// Retry-After: a full session, one that may only enrol, a wrong password, passwords forbidden, and
// too many attempts.
async function passwordAnswers() {
  const cases = [
    ["full", (b) => {
      check(visible(b, "signed-in") && text(b, "who") === "Signed in as " + answers.full.body.login + ".", "the page does not say who signed in: " + JSON.stringify(text(b, "who")));
      check(!visible(b, "password") && !visible(b, "passkey") && !visible(b, "enrolling") && !visible(b, "problem"), "the page offers another sign-in, or says something went wrong");
    }],
    ["enrolment", (b) => {
      check(visible(b, "enrolling") && visible(b, "signed-in") && /enrol/.test(text(b, "who")), "the page does not send the session to enrol a passkey");
      check(!visible(b, "password") && !visible(b, "passkey") && !visible(b, "problem"), "the page offers a sign-in to a session that may only enrol");
    }],
    ["wrong", (b) => {
      check(visible(b, "problem") && text(b, "problem").startsWith("That sign-in opens nothing"), "the page says " + JSON.stringify(text(b, "problem")));
      check(visible(b, "password") && visible(b, "passkey") && !visible(b, "signed-in"), "the page takes the sign-in away after a wrong password");
    }],
    ["forbidden", (b) => {
      check(visible(b, "problem") && /passwords are forbidden/i.test(text(b, "problem")), "the page says " + JSON.stringify(text(b, "problem")));
      check(!visible(b, "password") && visible(b, "passkey"), "the page offers the password again, or no passkey");
    }],
    ["tooMany", (b) => {
      const minutes = Math.ceil(Number(answers.tooMany.retryAfter) / 60);
      check(visible(b, "problem") && text(b, "problem").endsWith(" Try again in " + minutes + " minutes."), "the page says " + JSON.stringify(text(b, "problem")) + " for a wait of " + answers.tooMany.retryAfter + " seconds");
      check(visible(b, "password") && !visible(b, "signed-in"), "the page takes the password away after too many attempts");
    }],
  ];
  for (const [name, then] of cases) {
    const real = answers[name];
    scenario = "a password sign-in answered as the route answered " + name + " (" + real.status + ")";
    const b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
    all.push(b);
    await settle();
    request(b, "me").answer(401, { error: "this request carries no credential" });
    await settle();
    b.elements.login.value = real.login;
    b.elements.secret.value = "whatever was typed";
    await fire(b, "password", "submit");
    const sent = request(b, "auth/login");
    check(sent.body.login === real.login && sent.body.password === "whatever was typed", "the password sign-in sent " + JSON.stringify(sent.body));
    sent.answer(real.status, real.body, real.retryAfter ? { "Retry-After": real.retryAfter } : {});
    await settle();
    then(b);
  }
}

scenarios().then(
  () => say(JSON.stringify({ failures })),
  (e) => say(JSON.stringify({ failures: failures.concat(["the harness failed: " + (e && e.stack ? e.stack : e)]) })),
);
