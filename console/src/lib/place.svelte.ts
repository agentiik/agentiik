import { address, read, rootOf, type Route } from "./route";

// Where the console is: the route its address names, kept in step with the browser's history, so that
// back and forward move between screens and an address copied from the bar opens the same one.

export interface Where {
  readonly pathname: string;
  readonly search?: string;
  readonly baseURI: string;
}

export interface History {
  pushState(data: unknown, unused: string, url: string): void;
  replaceState(data: unknown, unused: string, url: string): void;
  // back and state are the browser's: what closing a screen opened as an entry of its own goes back
  // with, and what the entry holds, which says it was opened so.
  back?(): void;
  readonly state?: unknown;
}

// opened is what an entry holds where open made it, so that close knows back returns to the screen
// it narrowed rather than to wherever the console was before.
const opened = { opened: true };

export class Place {
  route = $state<Route>({ kind: "landing" });
  // query is the address's query, which holds what a screen is narrowed by: a filter is part of the
  // screen, so an address copied from the bar opens it filtered.
  query = $state(new URLSearchParams());
  readonly root: string;
  // baseURI is the console's, which every address of the API's is made from.
  readonly baseURI: string;
  readonly #where: Where;
  readonly #history: History;
  // leaving, where a screen sets it, is asked before the console moves away from the entry shown,
  // by a link or by back: it answers true to stay, where the screen holds what moving would lose,
  // and calls proceed itself once its user chooses to go all the same.
  leaving: ((proceed: () => void) => boolean) | null = null;
  // url and state are the entry shown, which a move leaving refused is put back to.
  #url: string;
  #state: unknown = null;

  constructor(where: Where, history: History) {
    this.#where = where;
    this.#history = history;
    this.root = rootOf(where.baseURI);
    this.baseURI = where.baseURI;
    this.route = read(where.pathname, this.root);
    this.query = new URLSearchParams(where.search ?? "");
    this.#url = where.pathname + (where.search ?? "");
    this.#state = history.state ?? null;
  }

  // href is the address of a route as a link writes it.
  href(route: Route): string {
    return new URL(address(route), this.#where.baseURI).pathname;
  }

  // go opens a route, as a new entry of the history or in place of the current one, with the query
  // given. A new entry is asked of leaving first, since it leaves the screen shown; one in place of
  // the current is the same screen narrowed, which loses nothing.
  go(route: Route, replace = false, query = new URLSearchParams()): void {
    if (!replace && this.leaving?.(() => this.go(route, replace, query))) {
      return;
    }
    this.#write(route, replace, query, null);
  }

  #write(route: Route, replace: boolean, query: URLSearchParams, state: unknown): void {
    const path = this.href(route);
    const written = query.toString();
    const url = written ? `${path}?${written}` : path;
    if (replace) {
      this.#history.replaceState(state, "", url);
    } else {
      this.#history.pushState(state, "", url);
    }
    this.#url = url;
    this.#state = state;
    this.route = read(path, this.root);
    this.query = new URLSearchParams(written);
  }

  // narrow changes the query of the screen shown, in place of the current entry of the history: a
  // filter changed is the same screen, which back should not step through key by key.
  narrow(query: URLSearchParams): void {
    this.go(this.route, true, query);
  }

  // open narrows the screen shown as a new entry of the history, so that back returns to it as it
  // was: the editor is opened so, and back closes it.
  open(query: URLSearchParams): void {
    this.#write(this.route, false, query, opened);
  }

  // close is what open opened closed, query being the screen without it: back, where open made the
  // entry shown, so that the history holds the screen once, and otherwise query in place of the
  // entry, for a screen opened from an address.
  close(query: URLSearchParams): void {
    const state = this.#history.state as { opened?: boolean } | null | undefined;
    if (state?.opened && this.#history.back) {
      this.#history.back();
      return;
    }
    this.narrow(query);
  }

  // moved is the browser having moved through its history, back or forward. Where leaving refuses
  // it, the entry shown is put back, and the move made again once its user chooses to go.
  moved(pathname: string, search = ""): void {
    if (this.leaving?.(() => this.#history.back?.())) {
      this.#history.pushState(this.#state, "", this.#url);
      return;
    }
    this.route = read(pathname, this.root);
    this.query = new URLSearchParams(search);
    this.#url = pathname + search;
    this.#state = this.#history.state ?? null;
  }
}

// follow is what a link does when clicked: the console opens the route itself rather than the browser
// loading the page again, unless the click asked for something else, a new tab or a download.
export function follow(place: Place, route: Route) {
  return (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    event.preventDefault();
    place.go(route);
  };
}
