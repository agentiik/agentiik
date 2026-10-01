import { getContext, setContext } from "svelte";
import type { components } from "../api/schema";

// The live connection, GET /api/v1/me/live: one WebSocket per tab, over which the API says what
// changed among what the caller reads, a run, the caller's notifications or the runners, naming it
// and never holding it. Each screen reads again what it shows when told, through the routes it read
// it from, rather than every few seconds; and reads everything again when the connection opens,
// since what changed while it was closed was said to nobody.

export type LiveChange = components["schemas"]["liveChange"];

// A socket, as WebSocket is one: the tests stand one in.
export interface Socket {
  onopen: ((ev: Event) => unknown) | null;
  onmessage: ((ev: MessageEvent<string>) => unknown) | null;
  onclose: ((ev: CloseEvent) => unknown) | null;
  close(code?: number, reason?: string): void;
}
export type Sockets = (url: string) => Socket;

// sockets is where connections come from: the browser's WebSocket, which carries the session cookie
// and the page's Origin to its own origin, or what a test stands in with socketsFrom.
let sockets: Sockets = (url) => new WebSocket(url) as unknown as Socket;

export function socketsFrom(s: Sockets): void {
  sockets = s;
}

// The waits before connecting again, longer each time to half a minute, so that an API restarting
// is found again within a second and one gone for good is not asked every second for ever.
const waits = [1000, 2000, 5000, 10000, 30000];

// settle is how long a screen waits after a change before it reads, so that what a run says in a
// burst, the API having gathered it a quarter of a second at a time, is read once.
const settle = 200;

// The close codes the API ends a connection with that say something of the caller: 1008, its
// credential no longer identifies it, which the console answers by reading who it is again.
const refused = 1008;

export class Live {
  // open is whether the connection is open.
  open = $state(false);

  readonly #url: string;
  readonly #ended: () => void;
  #socket: Socket | null = null;
  #tries = 0;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #running = false;
  #listeners = new Set<(change: LiveChange) => void>();

  // baseURI is the console's, the API's own origin; ended is called when the API closes the
  // connection because its credential is gone.
  constructor(baseURI: string, ended: () => void = () => {}) {
    const url = new URL("api/v1/me/live", baseURI);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    this.#url = url.href;
    this.#ended = ended;
  }

  start(): void {
    if (this.#running) return;
    this.#running = true;
    this.#connect();
  }

  stop(): void {
    this.#running = false;
    clearTimeout(this.#timer);
    const s = this.#socket;
    this.#socket = null;
    this.open = false;
    s?.close(1000, "the console closed");
  }

  #connect(): void {
    let s: Socket;
    try {
      s = sockets(this.#url);
    } catch {
      this.#later();
      return;
    }
    this.#socket = s;
    s.onopen = () => {
      if (this.#socket !== s) return;
      this.#tries = 0;
      this.open = true;
      this.#tell({ kind: "all" });
    };
    s.onmessage = (e) => {
      if (this.#socket !== s) return;
      try {
        this.#tell(JSON.parse(e.data) as LiveChange);
      } catch {
        this.#tell({ kind: "all" });
      }
    };
    s.onclose = (e) => {
      if (this.#socket !== s) return;
      this.#socket = null;
      this.open = false;
      if (e.code === refused) this.#ended();
      this.#later();
    };
  }

  #later(): void {
    if (!this.#running) return;
    const wait = waits[Math.min(this.#tries, waits.length - 1)]!;
    this.#tries++;
    this.#timer = setTimeout(() => {
      if (this.#running) this.#connect();
    }, wait);
  }

  #tell(change: LiveChange): void {
    for (const listener of [...this.#listeners]) listener(change);
  }

  // when calls read once the changes that matches picks, and every change of all, have settled,
  // until the function it answers is called.
  when(matches: (change: LiveChange) => boolean, read: () => void): () => void {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const listener = (change: LiveChange) => {
      if (change.kind !== "all" && !matches(change)) return;
      clearTimeout(timer);
      timer = setTimeout(read, settle);
    };
    this.#listeners.add(listener);
    return () => {
      clearTimeout(timer);
      this.#listeners.delete(listener);
    };
  }
}

// The console's one connection, handed to every screen through Svelte's context.
const key = Symbol("live");

export function provideLive(live: Live): void {
  setContext(key, live);
}

// useLive is the console's connection, or one that never connects where none was provided, as in a
// screen drawn alone.
export function useLive(): Live {
  return getContext<Live | undefined>(key) ?? new Live("http://localhost/");
}
