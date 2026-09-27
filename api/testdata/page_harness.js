// Run by TestThePageScriptSignsInEnrolsAndSignsOutOnAStandInBrowser, after codec.js, page.js wrapped
// in loadPage(), which runs it as a browser does on each page load, and the pages the test wrote:
// each element of a page by its id, hidden or not as the HTML says, and the page's data attributes.
// It stands in for the browser page.js reads, the DOM, fetch, navigator.credentials, location and
// history, drives each scenario a person would, answering each request as the API would, and
// prints, in one line of JSON, what went otherwise than a scenario says, and the QR codes the page
// drew, read back from its SVG, for the test to compare with a reference encoder's. It runs on node
// and on macOS's jsc, which prints with print and has no console and no URL.
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

// node is an element the page's script makes, as SVG's are: its attributes and its children.
function node(tag) {
  return {
    tag, attributes: {}, children: [],
    setAttribute(name, value) {
      this.attributes[name] = String(value);
    },
    appendChild(child) {
      this.children.push(child);
    },
    replaceChildren(...children) {
      this.children = children;
    },
  };
}

// stage loads a page, as the test wrote it, at an address, and answers its browser: one whose
// address carries hash after its #, which is not a secure context where insecure is set, and whose
// authenticator's credentials have no toJSON() where raw is set.
function stage(name, address, { hash, insecure, raw } = {}) {
  const written = pages[name];
  const elements = {};
  for (const id of Object.keys(written.elements)) {
    elements[id] = Object.assign(node(id), {
      id, hidden: written.elements[id].hidden, textContent: "", value: "", disabled: false, dataset: {}, listeners: {},
      addEventListener(type, f) {
        (this.listeners[type] = this.listeners[type] || []).push(f);
      },
    });
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
  global("document", {
    getElementById: (id) => elements[id] || null, baseURI: address,
    createElementNS: (ns, tag) => Object.assign(node(tag), { ns }),
  });
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

// drawnCodes are the QR codes the page drew, by the text it drew them for, as rows of 1 for dark.
const drawnCodes = {};

// drawnQR reads back the QR code the page drew in totp-qr: one SVG, a light square the size of its
// view box and the dark modules as runs of one row each, four modules in from its edge; and answers
// its rows, null where there is none.
function drawnQR(b) {
  const box = b.elements["totp-qr"];
  if (box.children.length !== 1) {
    return null;
  }
  const svg = box.children[0];
  check(svg.tag === "svg" && svg.ns === "http://www.w3.org/2000/svg", "the QR code is not SVG: " + svg.tag);
  const side = Number((svg.attributes.viewBox || "").split(" ")[2]);
  const [ground, dark] = svg.children;
  check(!!ground && ground.tag === "rect" && ground.attributes.fill === "#fff" && Number(ground.attributes.width) === side && Number(ground.attributes.height) === side,
    "the QR code's ground is " + JSON.stringify(ground && ground.attributes));
  check(!!dark && dark.tag === "path" && dark.attributes.fill === "#000", "the QR code's modules are " + JSON.stringify(dark && dark.attributes));
  const n = side - 8;
  const rows = Array.from({ length: n }, () => new Array(n).fill(0));
  const d = dark ? dark.attributes.d : "";
  const run = /M(\d+) (\d+)h(\d+)v1h-(\d+)z/g;
  let read = 0;
  for (let m = run.exec(d); m !== null; m = run.exec(d)) {
    read += m[0].length;
    const x = Number(m[1]) - 4;
    const y = Number(m[2]) - 4;
    check(m[3] === m[4] && x >= 0 && y >= 0 && y < n && x + Number(m[3]) <= n, "the QR code draws " + m[0] + " outside its modules");
    for (let i = 0; i < Number(m[3]) && y < n; i++) {
      rows[y][x + i] = 1;
    }
  }
  check(read === d.length, "the QR code's path holds what is not a run of modules");
  return rows.map((row) => row.join(""));
}

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
  b.elements.code.value = " " + code + " ";
  await fire(b, "enrol", "submit");
  const recovering = request(b, "auth/passkey/options");
  check(JSON.stringify(recovering.body) === JSON.stringify({ ceremony: "registration", code }), "the options asked with the code typed in were " + JSON.stringify(recovering.body));
  recovering.answer(200, registration);
  await settle();
  const recovered = request(b, "auth/passkey/verify");
  check(recovered.body.ceremony === "registration" && !("code" in recovered.body), "the verification sent " + JSON.stringify(recovered.body));
  recovered.answer(200, { ceremony: "registration", login: "alice", credential: { type: "passkey", id: "made", kind: "synced" } });
  await settle();
  check(b.elements.code.value === "" && visible(b, "enrolled") && /alice/.test(text(b, "enrolled-what")), "the page does not say the recovery code enrolled alice");
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  check(!visible(b, "code-field") && visible(b, "signed-in") && text(b, "who") === "Signed in as alice.", "the page asks for the spent code again, or does not say alice signed in");

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

  scenario = "an enrolment link, with a password";
  b = stage("enrol-password", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "enrol") && visible(b, "set-password") && !visible(b, "code-field") && /or set a password/.test(text(b, "intro")),
    "the page does not offer a passkey and a password from the link: " + JSON.stringify(text(b, "intro")));
  check(!visible(b, "own"), "the page offers the signed-in section to nobody");
  b.elements["new-password"].value = "erin's own passphrase";
  b.elements["new-password-again"].value = "erin's own passphrase, mistyped";
  await fire(b, "set-password", "submit");
  check(visible(b, "problem") && /differ/.test(text(b, "problem")) && !b.requests.some((q) => q.url.endsWith("auth/password/enrol")),
    "two passwords that differ were sent, or the page said nothing");
  check(b.elements["new-password"].value === "" && b.elements["new-password-again"].value === "", "the passwords are left in their fields");
  b.elements["new-password"].value = "erin's own passphrase";
  b.elements["new-password-again"].value = "erin's own passphrase";
  await fire(b, "set-password", "submit");
  let set = request(b, "auth/password/enrol");
  check(JSON.stringify(set.body) === JSON.stringify({ code, password: "erin's own passphrase" }) && set.init.method === "POST" && !set.url.includes(code),
    "the password set sent " + JSON.stringify(set.body) + " to " + set.url);
  set.answer(200, { login: "erin", session: "full", credential: { type: "password", id: "01M2AAZ9G62NQXFAFCXKRPJEH5", created_at: "2026-09-27T09:00:00Z" } });
  await settle();
  check(b.location.replaced === "/auth/enrol", "the spent code is left in the address: " + b.location.replaced);
  request(b, "me").answer(200, { principal: "erin" });
  await settle();
  check(visible(b, "signed-in") && text(b, "who") === "Signed in as erin." && text(b, "status") === "Password set for erin.", "the page does not say erin's password is set: " + JSON.stringify(text(b, "status")));
  check(!visible(b, "set-password") && !visible(b, "code-field") && visible(b, "enrol") && visible(b, "own") && visible(b, "own-more"),
    "the page does not offer erin a passkey and the signed-in section once her code is spent");

  scenario = "an enrolment link, with a password, where a passkey is required";
  b = stage("enrol-password", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  b.elements["new-password"].value = "erin's own passphrase";
  b.elements["new-password-again"].value = "erin's own passphrase";
  await fire(b, "set-password", "submit");
  request(b, "auth/password/enrol").answer(200, { login: "erin", session: "enrolment", credential: { type: "password", id: "x", created_at: "2026-09-27T09:00:00Z" } });
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(/passkey before anything else/.test(text(b, "status")) && visible(b, "enrol") && !visible(b, "set-password"), "the page does not send erin on to a passkey: " + JSON.stringify(text(b, "status")));
  check(visible(b, "own") && !visible(b, "own-more"), "a session that may only enrol is offered more than setting its password");

  scenario = "an enrolment link where passwords are forbidden to the account";
  b = stage("enrol-password", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  b.elements["new-password"].value = "erin's own passphrase";
  b.elements["new-password-again"].value = "erin's own passphrase";
  await fire(b, "set-password", "submit");
  request(b, "auth/password/enrol").answer(403, { error: "passwords are forbidden by the authentication policy that applies to this account, and none is set: enrol a passkey", setting: "password" });
  await settle();
  check(!visible(b, "set-password") && visible(b, "enrol") && /passwords are forbidden/i.test(text(b, "problem")), "the password is still offered once the API forbade it");
  check(b.location.replaced === null, "a refused password took the code out of the address");

  scenario = "an installation addressed by an IP address, with a password";
  b = stage("enrol-ip-password", "https://192.0.2.10/auth/enrol", { hash: "#" + code });
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "set-password") && !visible(b, "enrol") && visible(b, "unavailable") && /Set a password/.test(text(b, "intro")), "the page does not offer a password in place of the passkey");
  b.elements["new-password"].value = "a long enough passphrase";
  b.elements["new-password-again"].value = "a long enough passphrase";
  await fire(b, "set-password", "submit");
  check(request(b, "auth/password/enrol").body.code === code && !b.requests.some((q) => q.url.endsWith("auth/passkey/options")), "the password was not set from the link");
  await fire(b, "enrol", "submit");
  check(!b.requests.some((q) => q.url.endsWith("auth/passkey/options")), "a passkey ceremony was started where none runs");

  scenario = "a recovery code typed in, for a password";
  b = stage("enrol-password", "https://agentiik.example.com/auth/enrol");
  all.push(b);
  await settle();
  request(b, "me").answer(401, { error: "this request carries no credential" });
  await settle();
  check(visible(b, "code-field") && visible(b, "set-password"), "the page does not take a recovery code for a password");
  b.elements["new-password"].value = "a long enough passphrase";
  b.elements["new-password-again"].value = "a long enough passphrase";
  await fire(b, "set-password", "submit");
  check(visible(b, "problem") && !b.requests.some((q) => q.url.endsWith("auth/password/enrol")), "a password was sent with no code");
  b.elements.code.value = " " + code + " ";
  b.elements["new-password"].value = "a long enough passphrase";
  b.elements["new-password-again"].value = "a long enough passphrase";
  await fire(b, "set-password", "submit");
  check(request(b, "auth/password/enrol").body.code === code, "the code typed in was not sent");

  scenario = "the signed-in section: the password";
  b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  check(visible(b, "own") && visible(b, "own-more") && !visible(b, "totp-enrolling"), "the page offers alice no section setting her password");
  b.elements["current-password"].value = "correct horse battery staple";
  b.elements["changed-password"].value = "alice's new passphrase";
  b.elements["changed-password-again"].value = "alice's new passphrase";
  await fire(b, "change-password", "submit");
  const changed = request(b, "me/password");
  check(changed.init.method === "PUT" && JSON.stringify(changed.body) === JSON.stringify({ password: "alice's new passphrase", current_password: "correct horse battery staple" }),
    "the change sent " + changed.init.method + " " + JSON.stringify(changed.body));
  check(["current-password", "changed-password", "changed-password-again"].every((id) => b.elements[id].value === ""), "a password is left in its field");
  changed.answer(200, { type: "password", id: "alice-password", created_at: "2026-09-27T09:00:00Z" });
  await settle();
  check(/^Password set/.test(text(b, "status")), "the page does not say the password is set: " + JSON.stringify(text(b, "status")));
  b.elements["changed-password"].value = "alice's other passphrase";
  b.elements["changed-password-again"].value = "alice's other passphrase";
  await fire(b, "change-password", "submit");
  const first = request(b, "me/password");
  check(!("current_password" in first.body), "a current password left empty was sent: " + JSON.stringify(first.body));
  first.answer(429, { error: "too many password sign-ins were tried for this account or from this address in the last quarter of an hour: try again later, or sign in with a passkey" }, { "Retry-After": "600" });
  await settle();
  check(text(b, "problem").endsWith(" Try again in 10 minutes."), "the page says " + JSON.stringify(text(b, "problem")));
  await fire(b, "remove-password", "click");
  const removed = request(b, "me/password");
  check(removed.init.method === "DELETE" && removed.body === undefined, "the removal sent " + removed.init.method + " " + JSON.stringify(removed.body));
  removed.answer(204);
  await settle();
  request(b, "me").answer(401, { error: "that session opens nothing" });
  await settle();
  check(!visible(b, "own") && !visible(b, "signed-in") && visible(b, "password") && visible(b, "passkey") && /Password removed/.test(text(b, "status")),
    "once the session the password opened ended with it, the page does not offer a sign-in again");

  for (const [from, act] of [
    ["starting a generator", async (b) => {
      await fire(b, "totp-start", "click");
      return request(b, "me/totp");
    }],
    ["setting the password", async (b) => {
      b.elements["changed-password"].value = "alice's new passphrase";
      b.elements["changed-password-again"].value = "alice's new passphrase";
      await fire(b, "change-password", "submit");
      return request(b, "me/password");
    }],
  ]) {
    scenario = "the signed-in section where passwords are forbidden to the account, " + from;
    b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
    all.push(b);
    await settle();
    request(b, "me").answer(200, { principal: "alice" });
    await settle();
    (await act(b)).answer(403, { error: "passwords are forbidden by the authentication policy that applies to this account, and none is set: enrol a passkey", setting: "password" });
    await settle();
    check(!visible(b, "own") && /passwords are forbidden/i.test(text(b, "problem")), "the section is still offered once the API forbade passwords");
  }

  scenario = "the signed-in section: a one-time code generator";
  const uri = "otpauth://totp/Agentiik:alice@agentiik.example.com?algorithm=SHA1&digits=6&issuer=Agentiik&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP";
  b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  await fire(b, "totp-start", "click");
  const started = request(b, "me/totp");
  check(started.init.method === "POST" && started.body === undefined, "the generator was started with " + started.init.method + " " + JSON.stringify(started.body));
  started.answer(200, { id: "01M2AB3K5Q7R9T1V3X5Z7B9D1F", secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", uri, expires_at: "2026-09-27T09:10:00Z" });
  await settle();
  check(visible(b, "totp-enrolling") && !visible(b, "totp-start") && text(b, "totp-secret") === "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP" && text(b, "totp-uri") === uri,
    "the page does not show the key and its URI as text");
  drawnCodes[uri] = drawnQR(b);
  check(drawnCodes[uri] !== null, "the page drew no QR code");
  b.elements["totp-code"].value = " 492039 ";
  await fire(b, "totp-confirm", "submit");
  let confirmed = request(b, "me/totp/confirm");
  check(JSON.stringify(confirmed.body) === JSON.stringify({ totp: "492039" }), "the confirmation sent " + JSON.stringify(confirmed.body));
  confirmed.answer(422, { error: "that code is not the one the generator shows now: check that the device's clock is right, and send the code it shows next" });
  await settle();
  check(visible(b, "totp-enrolling") && visible(b, "problem") && text(b, "totp-secret") !== "", "a code refused took the key away, or said nothing");
  b.elements["totp-code"].value = "492040";
  await fire(b, "totp-confirm", "submit");
  request(b, "me/totp/confirm").answer(200, { type: "totp", id: "01M2AB3K5Q7R9T1V3X5Z7B9D1F", created_at: "2026-09-27T09:00:00Z" });
  await settle();
  check(!visible(b, "totp-enrolling") && visible(b, "totp-start") && text(b, "totp-secret") === "" && text(b, "totp-uri") === "" && drawnQR(b) === null && /One-time codes are on/.test(text(b, "status")),
    "the key is left on the page once the generator is on, or the page does not say so");
  b.elements["totp-remove-code"].value = "492041";
  await fire(b, "totp-remove", "submit");
  const gone = request(b, "me/totp");
  check(gone.init.method === "DELETE" && JSON.stringify(gone.body) === JSON.stringify({ totp: "492041" }), "the removal sent " + gone.init.method + " " + JSON.stringify(gone.body));
  gone.answer(204);
  await settle();
  check(/removed/.test(text(b, "status")), "the page does not say the generator is removed");

  scenario = "the signed-in section, for a session that may only enrol";
  b = stage("sign-in-password", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(403, { error: "this session enrols passkeys and nothing else" });
  await settle();
  check(visible(b, "own") && visible(b, "change-password") && !visible(b, "own-more"), "a session that may only enrol is not offered its password alone");

  scenario = "the signed-in section where passwords are withheld";
  b = stage("sign-in", "https://agentiik.example.com/auth/sign-in");
  all.push(b);
  await settle();
  request(b, "me").answer(200, { principal: "alice" });
  await settle();
  check(!visible(b, "own"), "the page offers a password where the installation withholds them");

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

  // The enrolment page's password, each answer of POST /api/v1/auth/password/enrol followed by what
  // GET /api/v1/me would answer the session it opened.
  const enrolled = [
    ["enrolFull", [200, { principal: "erin" }], (b) => {
      check(text(b, "status") === "Password set for erin." && visible(b, "signed-in") && text(b, "who") === "Signed in as erin.", "the page says " + JSON.stringify(text(b, "status")));
      check(visible(b, "own") && visible(b, "own-more") && !visible(b, "set-password") && !visible(b, "problem"), "the page does not offer erin the signed-in section");
    }],
    ["enrolEnrolment", [403, { error: "this session enrols passkeys and nothing else" }], (b) => {
      check(/passkey before anything else/.test(text(b, "status")) && visible(b, "enrol") && !visible(b, "set-password"), "the page does not send frank on to a passkey: " + JSON.stringify(text(b, "status")));
    }],
    ["enrolForbidden", null, (b) => {
      check(!visible(b, "set-password") && visible(b, "enrol") && /passwords are forbidden/i.test(text(b, "problem")), "the page says " + JSON.stringify(text(b, "problem")) + " and offers the password again");
    }],
  ];
  for (const [name, me, then] of enrolled) {
    const real = answers[name];
    scenario = "a password set from a link answered as the route answered " + name + " (" + real.status + ")";
    const b = stage("enrol-password", "https://agentiik.example.com/auth/enrol", { hash: "#" + code });
    all.push(b);
    await settle();
    request(b, "me").answer(401, { error: "this request carries no credential" });
    await settle();
    b.elements["new-password"].value = real.login + "'s own passphrase";
    b.elements["new-password-again"].value = real.login + "'s own passphrase";
    await fire(b, "set-password", "submit");
    request(b, "auth/password/enrol").answer(real.status, real.body);
    await settle();
    if (me) {
      request(b, "me").answer(me[0], me[1]);
      await settle();
    }
    then(b);
  }
}

scenarios().then(
  () => say(JSON.stringify({ failures, drawn: drawnCodes })),
  (e) => say(JSON.stringify({ failures: failures.concat(["the harness failed: " + (e && e.stack ? e.stack : e)]), drawn: drawnCodes })),
);
