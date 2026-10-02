import { socketsFrom, type LiveChange, type Socket } from "../src/lib/live.svelte";

// A stand-in for the live connection: every socket the console opens is kept here, and a test opens
// it, says a change on it or ends it, as the API would.
export class FakeSocket implements Socket {
  onopen: ((ev: Event) => unknown) | null = null;
  onmessage: ((ev: MessageEvent<string>) => unknown) | null = null;
  onclose: ((ev: CloseEvent) => unknown) | null = null;
  closed = false;

  constructor(readonly url: string) {}

  close(): void {
    this.closed = true;
  }

  open(): void {
    this.onopen?.(new Event("open"));
  }

  say(change: LiveChange): void {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(change) }));
  }

  end(code: number): void {
    this.onclose?.({ code } as CloseEvent);
  }
}

export const sockets: FakeSocket[] = [];

// standIn makes every socket the console opens a FakeSocket, which never opens by itself.
export function standIn(): void {
  sockets.length = 0;
  socketsFrom((url) => {
    const s = new FakeSocket(url);
    sockets.push(s);
    return s;
  });
}

// opened is the console's socket once it has one, opened.
export function opened(): FakeSocket {
  const s = sockets.at(-1);
  if (!s) throw new Error("the console opened no live connection");
  s.open();
  return s;
}
