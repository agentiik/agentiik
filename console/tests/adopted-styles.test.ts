import { StyleModule } from "style-mod";
import { describe, expect, it } from "vitest";
import { adoptStyleModules } from "../src/lib/editor/adopted-styles";

// The document the tests draw in adopts no stylesheet, which a browser's does: it is given the
// property, so that what CodeMirror's styles become under the console's policy is what is tested.
function adopting() {
  const doc = document.implementation.createHTMLDocument("editor");
  let adopted: CSSStyleSheet[] = [];
  Object.defineProperty(doc, "adoptedStyleSheets", { get: () => adopted, set: (sheets: CSSStyleSheet[]) => (adopted = sheets), configurable: true });
  const selectors = () => adopted.flatMap((sheet) => [...sheet.cssRules].map((r) => (r as CSSStyleRule).selectorText));
  return { doc, adopted: () => adopted, selectors };
}

describe("CodeMirror's styles under the console's policy", () => {
  adoptStyleModules();
  const base = new StyleModule({ ".base": { color: "red" } });
  const theme = new StyleModule({ ".theme": { color: "blue" } });
  const panel = new StyleModule({ ".panel": { color: "green" } });

  it("are a stylesheet the document adopts, never a style element written into it", () => {
    const { doc, adopted, selectors } = adopting();
    StyleModule.mount(doc, [base, theme]);
    StyleModule.mount(doc, [base, theme]);
    expect(doc.querySelectorAll("style")).toHaveLength(0);
    expect(adopted()).toHaveLength(1);
    expect(selectors()).toEqual([".base", ".theme"]);
  });

  it("keep the order style-mod keeps, each module after the one given before it", () => {
    const { doc, selectors } = adopting();
    StyleModule.mount(doc, [base, theme]);
    StyleModule.mount(doc, [panel, base]);
    expect(selectors()).toEqual([".panel", ".base", ".theme"]);
    // A module mounted before one it is now given after is moved after it.
    StyleModule.mount(doc, [theme, base]);
    expect(selectors()).toEqual([".panel", ".theme", ".base"]);
  });

  it("are left to style-mod where the document adopts no stylesheet", () => {
    const doc = document.implementation.createHTMLDocument("plain");
    StyleModule.mount(doc, [base]);
    expect(doc.querySelector("style")?.textContent).toContain(".base");
  });
});
