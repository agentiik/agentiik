import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, type Completion, type CompletionContext, type CompletionResult } from "@codemirror/autocomplete";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import { bracketMatching, HighlightStyle, indentUnit, syntaxHighlighting } from "@codemirror/language";
import { lintGutter, type Diagnostic } from "@codemirror/lint";
import { highlightSelectionMatches, search, searchKeymap } from "@codemirror/search";
import { EditorState, type Extension, type Text } from "@codemirror/state";
import { drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, hoverTooltip, keymap, lineNumbers } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { adoptStyleModules } from "./adopted-styles";
import { describeAt, keysAt, placeAt, siblingsAt, valuesAt } from "./schema-help";

// The YAML editor's CodeMirror: YAML read as the language writes it, the keys and values
// workflow.schema.json takes offered as they are typed and described under the pointer, the file's
// problems marked on their lines, and git's own two-space indentation. Its colours are the design's
// tokens, read from the page as it draws, so that it follows the ground the console is in.

adoptStyleModules();

// complete offers the keys the map being typed in takes, less those it already holds, or the values
// a key takes where the schema lists them.
function complete(ctx: CompletionContext): CompletionResult | null {
  const text = ctx.state.doc.toString();
  const place = placeAt(text, ctx.pos);
  if (!ctx.explicit && place.word === "" && !ctx.matchBefore(/:\s$/)) return null;
  let options: Completion[];
  if (place.key) {
    options = valuesAt([...place.path, place.key]).map((v) => ({ label: v, type: "constant" }));
  } else {
    const held = siblingsAt(text, ctx.pos);
    options = keysAt(place.path)
      .filter((k) => !held.has(k.name))
      .map((k) => ({ label: k.name, type: "property", info: k.description || undefined, apply: `${k.name}: ` }));
  }
  if (options.length === 0) return null;
  return { from: place.from, options, validFor: /^[\w.-]*$/ };
}

// describe is what the key under the pointer is for, as the schema says it.
const describe = hoverTooltip((view, pos) => {
  const line = view.state.doc.lineAt(pos);
  const m = /^(\s*(?:-\s+)?)([A-Za-z0-9_.$-]+)\s*:/.exec(line.text);
  if (!m) return null;
  const from = line.from + m[1]!.length;
  const to = from + m[2]!.length;
  if (pos < from || pos > to) return null;
  const place = placeAt(view.state.doc.toString(), to);
  const said = describeAt([...place.path, m[2]!]);
  if (!said) return null;
  return {
    pos: from,
    end: to,
    above: true,
    create: () => {
      const dom = document.createElement("div");
      dom.className = "cm-described";
      const name = document.createElement("strong");
      name.textContent = m[2]!;
      const text = document.createElement("p");
      text.textContent = said;
      dom.append(name, text);
      return { dom };
    },
  };
});

const highlight = HighlightStyle.define([
  { tag: [tags.propertyName, tags.definition(tags.propertyName)], color: "var(--accent)" },
  { tag: tags.comment, color: "var(--faint)" },
  { tag: [tags.punctuation, tags.separator, tags.squareBracket, tags.brace], color: "var(--muted)" },
  { tag: [tags.labelName, tags.typeName, tags.meta], color: "var(--muted)" },
]);

// Every line of the console is a whole number of units high, from the leading each element turns
// into units at its own size (styles/base.css): 1.6 at the code's 12.5px is the code's 20px line.
const theme = EditorView.theme({
  "&": { color: "var(--text)", backgroundColor: "var(--sunken)", borderRadius: "var(--radius-control)", fontSize: "var(--type-code-size)", "--leading": "1.6" },
  "&.cm-focused": { outline: "var(--border-focus) solid var(--accent)", outlineOffset: "1px" },
  ".cm-scroller": { fontFamily: "var(--type-code-font)" },
  ".cm-content": { padding: "6px 0", caretColor: "var(--text)" },
  ".cm-line": { padding: "0 12px 0 6px" },
  ".cm-gutters": { backgroundColor: "var(--sunken)", color: "var(--faint)", border: "none", borderTopLeftRadius: "var(--radius-control)", borderBottomLeftRadius: "var(--radius-control)" },
  ".cm-lineNumbers .cm-gutterElement": { padding: "0 6px 0 12px", minWidth: "24px" },
  ".cm-activeLine": { backgroundColor: "color-mix(in srgb, var(--accentDim) 45%, transparent)" },
  ".cm-activeLineGutter": { backgroundColor: "transparent", color: "var(--text)" },
  ".cm-cursor": { borderLeftColor: "var(--text)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": { backgroundColor: "var(--accentDim) !important" },
  ".cm-selectionMatch": { backgroundColor: "color-mix(in srgb, var(--accentDim) 60%, transparent)" },
  ".cm-matchingBracket": { backgroundColor: "var(--accentDim)", outline: "none" },
  ".cm-lintRange-error": { backgroundImage: "none", textDecoration: "underline wavy var(--failed)", textDecorationSkipInk: "none", textUnderlineOffset: "3px" },
  ".cm-gutter-lint": { width: "14px" },
  ".cm-lint-marker": { width: "10px", height: "10px" },
  ".cm-tooltip": { backgroundColor: "var(--raised)", color: "var(--text)", border: "var(--border-hairline) solid var(--lineStrong)", borderRadius: "var(--radius-control)" },
  ".cm-tooltip-autocomplete > ul": { fontFamily: "var(--type-code-font)", maxHeight: "16em" },
  ".cm-tooltip-autocomplete > ul > li": { padding: "2px 8px" },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": { backgroundColor: "var(--accentDim)", color: "var(--text)" },
  ".cm-completionInfo": { maxWidth: "360px", padding: "6px 10px", fontFamily: "var(--type-body-font, inherit)" },
  ".cm-completionIcon": { display: "none" },
  ".cm-described": { maxWidth: "380px", padding: "6px 10px", fontSize: "13px" },
  ".cm-described p": { margin: "4px 0 0", color: "var(--muted)" },
  ".cm-tooltip-lint": { padding: "0" },
  ".cm-diagnostic-error": { borderLeftColor: "var(--failed)" },
  ".cm-panels": { backgroundColor: "var(--raised)", color: "var(--text)" },
  ".cm-panels.cm-panels-bottom": { borderTop: "var(--border-hairline) solid var(--line)" },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, var(--waiting) 30%, transparent)" },
  ".cm-searchMatch-selected": { backgroundColor: "color-mix(in srgb, var(--waiting) 55%, transparent)" },
});

export type Options = { label: string };

export function extensions(o: Options, changed: (state: EditorState) => void): Extension[] {
  return [
    lineNumbers(),
    highlightActiveLineGutter(),
    lintGutter(),
    history(),
    drawSelection(),
    EditorState.allowMultipleSelections.of(true),
    EditorState.tabSize.of(2),
    indentUnit.of("  "),
    yaml(),
    syntaxHighlighting(highlight),
    bracketMatching(),
    closeBrackets(),
    autocompletion({ override: [complete], icons: false }),
    describe,
    highlightActiveLine(),
    highlightSelectionMatches(),
    search({ top: false }),
    keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...completionKeymap, indentWithTab]),
    EditorView.contentAttributes.of({ "aria-label": o.label, spellcheck: "false", autocapitalize: "off", autocorrect: "off" }),
    EditorView.lineWrapping,
    theme,
    EditorView.updateListener.of((u) => {
      if (u.docChanged) changed(u.state);
    }),
  ];
}

export type Marked = { line: number; column?: number; message: string };

// diagnostics is the problems of the file on their lines: from the column named, or from the line's
// first character, to its end.
export function diagnostics(doc: Text, problems: Marked[]): Diagnostic[] {
  return problems.flatMap((p) => {
    if (p.line < 1 || p.line > doc.lines) return [];
    const line = doc.line(p.line);
    const lead = line.text.length - line.text.trimStart().length;
    const from = line.from + Math.min(line.length, p.column ? p.column - 1 : lead);
    const to = Math.max(from + 1 > line.to ? from : line.to, from);
    return [{ from, to, severity: "error" as const, message: p.message }];
  });
}
