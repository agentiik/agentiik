import { StyleModule } from "style-mod";

// CodeMirror draws its styles with style-mod, which mounts them into a document as a style element
// in its head, and the console's policy refuses any style written into the page (style-src 'self'):
// the editor would be drawn with none of them. A stylesheet built in script and adopted by the
// document is what the policy lets through, and what style-mod itself uses in a shadow root; so a
// document is given one, holding the modules mounted in it in the order style-mod keeps them, so that
// a theme still comes after the base it refines. A browser with no adopted stylesheets keeps
// style-mod's own way.

type Mounted = { sheet: CSSStyleSheet; modules: StyleModule[] };

const sets = new WeakMap<Document, Mounted>();

function adopts(root: unknown): root is Document {
  return typeof Document !== "undefined" && root instanceof Document && "adoptedStyleSheets" in root && typeof CSSStyleSheet === "function" && "replaceSync" in CSSStyleSheet.prototype;
}

export function adoptStyleModules() {
  const own = StyleModule.mount;
  if ((own as { adopted?: true }).adopted) return;
  const mount: typeof StyleModule.mount = (root, modules, options) => {
    if (!adopts(root)) return own.call(StyleModule, root, modules, options);
    let set = sets.get(root);
    if (!set) {
      set = { sheet: new CSSStyleSheet(), modules: [] };
      sets.set(root, set);
    }
    // style-mod's order: each module given comes after the one given before it, and one already
    // mounted out of that order is moved.
    let changed = false;
    let j = 0;
    for (const mod of Array.isArray(modules) ? modules : [modules as StyleModule]) {
      let index = set.modules.indexOf(mod);
      if (index > -1 && index < j) {
        set.modules.splice(index, 1);
        j--;
        index = -1;
      }
      if (index === -1) {
        set.modules.splice(j++, 0, mod);
        changed = true;
      } else {
        j = index + 1;
      }
    }
    if (changed) set.sheet.replaceSync(set.modules.map((m) => m.getRules()).join("\n"));
    if (!root.adoptedStyleSheets.includes(set.sheet)) root.adoptedStyleSheets = [set.sheet, ...root.adoptedStyleSheets];
  };
  (mount as { adopted?: true }).adopted = true;
  StyleModule.mount = mount;
}
