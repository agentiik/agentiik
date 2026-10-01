import type { Place } from "./place.svelte";
import { presets, queryOfRange, rangeOf, type Preset, type Range } from "./stats";

// The range every chart of a statistics page is drawn over, read from the address and written back
// to it, and the zooms that narrowed it: "dragging across a chart narrows the range to that span,
// for every chart; a double click steps back". Each zoom is kept, so that the range before it can
// be gone back to, and choosing a preset starts over.
//
// It is given the place to read as a function, as a component's props are read, so that it reads
// the one in use rather than the one it was made with.
export class Ranged {
  #place: () => Place;
  before = $state<Range[]>([]);
  range = $derived.by(() => rangeOf(this.place.query, Date.now()));

  constructor(place: () => Place) {
    this.#place = place;
  }

  get place(): Place {
    return this.#place();
  }

  narrow(change: Partial<Range>, keep = true) {
    if (keep) this.before = [...this.before, this.range];
    this.place.narrow(queryOfRange({ ...this.range, ...change }, this.place.query));
  }

  choose(preset: Preset) {
    this.before = [];
    const now = Date.now();
    this.narrow({ preset, from: new Date(now - presets[preset].ms), to: new Date(now) }, false);
  }

  zoom(from: Date, to: Date) {
    this.narrow({ from, to, preset: undefined });
  }

  back() {
    const last = this.before.at(-1);
    if (!last) return;
    this.before = this.before.slice(0, -1);
    this.place.narrow(queryOfRange(last, this.place.query));
  }

  compare(on: boolean) {
    this.narrow({ compare: on }, false);
  }
}

// query is a range as every statistics route takes it.
export function query(r: Range) {
  return { from: r.from.toISOString(), to: r.to.toISOString(), compare: r.compare ? ("previous" as const) : undefined };
}
