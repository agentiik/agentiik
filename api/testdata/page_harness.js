// Run by TestThePageScriptOnAStandInBrowser, after codec.js, page.js wrapped in loadPage(), which
// runs it as a browser does on each page load, and the pages the test wrote: each element of a page
// by its id, hidden or not as the HTML says, and the page's data attributes. It stands in for the
// browser page.js reads, the DOM, fetch, navigator.credentials, location and history, drives each
// scenario a person would, answering each request as the API would, and prints, in one line of JSON,
// what went otherwise than a scenario says. It runs on node and on macOS's jsc, which prints with
// print and has no console and no URL.
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

// stage loads a page, as the test wrote it, at an address, and answers its browser.
function stage(name, address, hash) {
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
    isSecureContext: true, PublicKeyCredential, location: browser.location,
    history: { replaceState: (_, __, to) => { browser.location.replaced = to; } },
  });
  global("PublicKeyCredential", PublicKeyCredential);
  global("navigator", {
    credentials: {
      get: async (options) => {
        browser.got.push(options);
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
      answer(status, json) {
        this.answered = true;
        resolve({ status, json: async () => { if (json === undefined) { throw new SyntaxError("no body"); } return json; } });
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
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(visible(b, "enrolling"), "the page does not say the account needs a passkey first");
  check(visible(b, "signed-in") && /enrol/.test(text(b, "who")), "the page offers no sign-out: " + JSON.stringify(text(b, "who")));
  await fire(b, "sign-out", "click");
  const out = request(b, "auth/sign-out");
  check(out.init.method === "POST", "the sign-out is sent as " + out.init.method);
  out.answer(204);
  await settle();
  check(!visible(b, "signed-in") && !visible(b, "enrolling") && text(b, "status") === "Signed out.", "the page did not sign out: " + text(b, "status"));

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
  b = stage("enrol", "https://agentiik.example.com/auth/enrol", "#" + code);
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

  scenario = "every request";
  for (const browser of all) {
    for (const q of browser.requests) {
      check(q.url.startsWith("https://agentiik.example.com/api/v1/"), "a request went to " + q.url);
      check(q.init.credentials === "same-origin" && q.init.mode === undefined, "a request to " + q.url + " was sent with credentials " + q.init.credentials + " and mode " + q.init.mode);
    }
  }
}

scenarios().then(
  () => say(JSON.stringify({ failures })),
  (e) => say(JSON.stringify({ failures: failures.concat(["the harness failed: " + (e && e.stack ? e.stack : e)]) })),
);
