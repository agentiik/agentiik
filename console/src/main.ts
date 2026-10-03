import "./styles/fonts.css";
import "../vendor/tokens.css";
import "./styles/base.css";
import { mount } from "svelte";
import { connect } from "./api/client";
import App from "./App.svelte";
import { elsewhere, openedIn } from "./lib/origin";
import { Place } from "./lib/place.svelte";
import { Session } from "./lib/session.svelte";
import { passkeysUnavailable } from "./lib/signin";
import { apply, chosen } from "./lib/theme";

// The console, drawn into the page the API served, against the API that served it.

apply(document.documentElement, globalThis.localStorage, chosen(globalThis.localStorage));

const target = document.getElementById("console");
if (target) {
  const place = new Place({ pathname: window.location.pathname, search: window.location.search, baseURI: document.baseURI }, window.history);
  window.addEventListener("popstate", () => place.moved(window.location.pathname, window.location.search));
  const api = connect(document.baseURI);
  const unavailable = passkeysUnavailable(window);
  const passkeys = { unavailable, credentials: unavailable ? undefined : navigator.credentials };
  mount(App, { target, props: { api, session: new Session(api), place, version: __AGENTIIK_VERSION__, passkeys, elsewhere: elsewhere(openedIn(document, window.location)) } });
}
