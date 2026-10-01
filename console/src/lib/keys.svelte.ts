import { getContext, setContext, untrack } from "svelte";

// The keys of the view, which the key line at the foot of the window names: "the keys of the view,
// each named by its effect, as agk console's bottom line names them", shortcuts beside the buttons
// and links that do the same, never the only way. Each view declares its own while it is drawn, the
// console adds those of every view, and a key or an act the principal does not hold is left out,
// as the button it stands beside is.

// A Binding is one act: the keys that do it, as KeyboardEvent.key spells them, what it does, named
// as the button beside it is, and the act itself, told which of its keys was pressed. brief is the
// keys the line draws where it has no room for every one, the arrows without j and k, and inLine
// false keeps the act out of the line, listed only with every key of the view.
export type Binding = { keys: string[]; brief?: string[]; effect: string; does: (key: string) => void; inLine?: boolean };

// shown is a key as the key line writes it: an arrow as its arrow, and a named key in lower case,
// as agk console writes enter and esc.
export function shown(key: string): string {
  return { ArrowUp: "↑", ArrowDown: "↓", ArrowLeft: "←", ArrowRight: "→", Enter: "enter", Escape: "esc" }[key] ?? key;
}

// Where a key belongs to what has the focus rather than to the view: a field typed into, and the
// keys a focused control acts on itself, enter on a button or a link, which would otherwise act
// twice.
function typedInto(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName);
}

function actsOnItsOwn(target: EventTarget | null, key: string): boolean {
  if (!(target instanceof HTMLElement) || (key !== "Enter" && key !== " ")) return false;
  return ["BUTTON", "A", "SUMMARY"].includes(target.tagName) || target.getAttribute("role") === "tab";
}

export class Keys {
  // The layers of bindings, one for each view drawn, the last drawn first, and then the console's
  // own: a view's key wins where both name it.
  #layers = $state.raw<{ bindings: () => Binding[] }[]>([]);
  #own: () => Binding[];

  constructor(own: () => Binding[] = () => []) {
    this.#own = own;
  }

  // listing is whether every key of the view is listed, which ? opens and esc closes.
  listing = $state(false);

  get bindings(): Binding[] {
    return [...[...this.#layers].reverse().flatMap((l) => l.bindings()), ...this.#own()];
  }

  // use declares a view's keys for as long as it is drawn, read again whenever what they read
  // changes, so that an act the view stops offering leaves the line with its button.
  use(bindings: () => Binding[]) {
    const layer = { bindings };
    $effect(() => {
      untrack(() => (this.#layers = [...this.#layers, layer]));
      return () => {
        this.#layers = this.#layers.filter((l) => l !== layer);
      };
    });
  }

  // press does what a key pressed in the window names, and says whether it named anything.
  press(e: KeyboardEvent): boolean {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return false;
    if (typedInto(e.target) || actsOnItsOwn(e.target, e.key)) return false;
    if (this.listing && e.key === "Escape") {
      this.listing = false;
      e.preventDefault();
      return true;
    }
    const found = this.bindings.find((b) => b.keys.includes(e.key));
    if (!found) return false;
    e.preventDefault();
    found.does(e.key);
    return true;
  }
}

const context = Symbol("keys");

// provide makes keys the console's, for every view it draws.
export function provide(keys: Keys): Keys {
  setContext(context, keys);
  return keys;
}

// useKeys declares a view's keys with the console's, and with nothing where a view is drawn alone,
// as a test draws one.
export function useKeys(bindings: () => Binding[]) {
  const keys = getContext<Keys | undefined>(context);
  keys?.use(bindings);
}

// moved is the item an arrow, or j or k, moves the selection to among items: the next or the one
// before, the first where nothing is selected yet, and the same one at either end.
export function moved<T>(items: T[], current: T | undefined, key: string): T | undefined {
  if (items.length === 0) return undefined;
  const i = current === undefined ? -1 : items.indexOf(current);
  const down = key === "ArrowDown" || key === "j";
  if (i < 0) return down ? items[0] : items[items.length - 1];
  return items[Math.min(items.length - 1, Math.max(0, i + (down ? 1 : -1)))];
}
