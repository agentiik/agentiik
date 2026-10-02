// How wide the window is, as the frame lays itself out by it. From 1100px the sidebar is drawn whole,
// unless its reader folded it; under 1100px it is folded to its icons, so that a table keeps the
// width it needs; under 760px, a phone's or a narrow window's, it leaves the screen altogether and
// opens over it from a button in the top bar, and the key line, which names keys a phone has none
// of, is not drawn.
//
// The widths are the ones the media queries of the screens use, written there again since a media
// query reads no variable.

export const compactBelow = 1100;
export const narrowBelow = 760;

export class Viewport {
  compact = $state(false);
  narrow = $state(false);

  constructor() {
    this.#watch(`(max-width: ${compactBelow - 1}px)`, (v) => (this.compact = v));
    this.#watch(`(max-width: ${narrowBelow - 1}px)`, (v) => (this.narrow = v));
  }

  #watch(query: string, set: (matches: boolean) => void) {
    if (typeof matchMedia !== "function") return;
    const m = matchMedia(query);
    set(m.matches);
    m.addEventListener?.("change", (e) => set(e.matches));
  }
}
