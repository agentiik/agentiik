import { cleanup, configure } from "@testing-library/svelte";
import { afterEach, beforeEach } from "vitest";
import { standIn } from "./live";

// What a browser gives a page and the document the tests draw in does not: uPlot asks for
// matchMedia as it loads, and a chart watches its size with a ResizeObserver. Neither does anything
// here, since nothing is laid out. Each is given to the global scope, which is what the modules read.

const scope = globalThis as unknown as Record<string, unknown>;

if (typeof scope.matchMedia !== "function") {
  scope.matchMedia = (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  });
}

if (typeof scope.ResizeObserver !== "function") {
  scope.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}

// CodeMirror measures what it draws, which a document with no layout cannot: a range answers no
// rectangle, and every box has no size.
const rects = { length: 0, item: () => null, [Symbol.iterator]: function* () {} };
const box = { x: 0, y: 0, top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0, toJSON() {} };
if (typeof Range !== "undefined") {
  Range.prototype.getClientRects ??= () => rects as unknown as DOMRectList;
  Range.prototype.getBoundingClientRect ??= () => box as DOMRect;
}

// Each screen is a chunk of its own, imported when it is first shown, which a machine running every
// file at once may take longer than the second testing-library waits by default to transform: a
// query that waits for a screen waits five.
configure({ asyncUtilTimeout: 5000 });

// The live connection is a stand-in in every test, which a test opens and speaks on: the document the
// tests draw in would otherwise try to reach the stand-in address over the network.
beforeEach(() => standIn());

// Each test draws the console into the one document, and a console left behind by the test before
// would answer the next one's queries: every test's is taken away when it ends.
afterEach(() => cleanup());
