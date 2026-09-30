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
}

export class Place {
  route = $state<Route>({ kind: "landing" });
  // query is the address's query, which holds what a screen is narrowed by: a filter is part of the
  // screen, so an address copied from the bar opens it filtered.
  query = $state(new URLSearchParams());
  readonly root: string;
  readonly #where: Where;
  readonly #history: History;

  constructor(where: Where, history: History) {
    this.#where = where;
    this.#history = history;
    this.root = rootOf(where.baseURI);
    this.route = read(where.pathname, this.root);
    this.query = new URLSearchParams(where.search ?? "");
  }

  // href is the address of a route as a link writes it.
  href(route: Route): string {
    return new URL(address(route), this.#where.baseURI).pathname;
  }

  // go opens a route, as a new entry of the history or in place of the current one, with the query
  // given or none.
  go(route: Route, replace = false, query = new URLSearchParams()): void {
    const path = this.href(route);
    const written = query.toString();
    const url = written ? `${path}?${written}` : path;
    if (replace) {
      this.#history.replaceState(null, "", url);
    } else {
      this.#history.pushState(null, "", url);
    }
    this.route = read(path, this.root);
    this.query = new URLSearchParams(written);
  }

  // narrow changes the query of the screen shown, in place of the current entry of the history: a
  // filter changed is the same screen, which back should not step through key by key.
  narrow(query: URLSearchParams): void {
    this.go(this.route, true, query);
  }

  // moved is the browser having moved through its history, back or forward.
  moved(pathname: string, search = ""): void {
    this.route = read(pathname, this.root);
    this.query = new URLSearchParams(search);
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
