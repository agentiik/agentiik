import { cleanup } from "@testing-library/svelte";
import { afterEach } from "vitest";

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

// Each test draws the console into the one document, and a console left behind by the test before
// would answer the next one's queries: every test's is taken away when it ends.
afterEach(() => cleanup());
