<script lang="ts">
  import { redo as redoCommand, redoDepth, undo as undoCommand, undoDepth } from "@codemirror/commands";
  import { setDiagnostics } from "@codemirror/lint";
  import { EditorSelection, EditorState } from "@codemirror/state";
  import { EditorView } from "@codemirror/view";
  import { onMount, untrack } from "svelte";
  import { diagnostics, extensions, type Marked } from "../lib/editor/codemirror";

  // The file as it is typed, in CodeMirror: one history of every change, typed or made on the graph,
  // which Undo walks back; the file's problems marked on their lines; and what the text holds told to
  // whoever holds the model, at each change.
  let {
    value,
    label,
    problems = [],
    onchange,
  }: {
    value: string;
    label: string;
    problems?: Marked[];
    onchange: (text: string, history: { undo: number; redo: number }) => void;
  } = $props();

  let holder = $state<HTMLDivElement | undefined>();
  let view: EditorView | undefined;

  const told = (state: EditorState) => onchange(state.doc.toString(), { undo: undoDepth(state), redo: redoDepth(state) });

  const fresh = (doc: string) => EditorState.create({ doc, extensions: extensions({ label: untrack(() => label) }, told) });

  onMount(() => {
    view = new EditorView({ parent: holder!, state: fresh(untrack(() => value)) });
    return () => view?.destroy();
  });

  // A value given from outside, an edit of the graph, is made in the editor as the least change
  // that turns its text into it, so that the cursor and the history stay where they were.
  $effect(() => {
    const next = value;
    const v = view;
    if (!v) return;
    const now = v.state.doc.toString();
    if (now === next) return;
    let start = 0;
    while (start < now.length && start < next.length && now[start] === next[start]) start++;
    let end = 0;
    while (end < now.length - start && end < next.length - start && now[now.length - 1 - end] === next[next.length - 1 - end]) end++;
    v.dispatch({ changes: { from: start, to: now.length - end, insert: next.slice(start, next.length - end) }, userEvent: "input.graph" });
  });

  $effect(() => {
    const marked = problems;
    const v = view;
    if (!v) return;
    v.dispatch(setDiagnostics(v.state, diagnostics(v.state.doc, marked)));
  });

  // reset starts the editor again on a text, with no history behind it: every change undone at once
  // leaves nothing for Undo to walk back.
  export function reset(doc: string) {
    if (!view) return;
    view.setState(fresh(doc));
    told(view.state);
  }

  export function undo() {
    if (view) undoCommand(view);
  }

  export function redo() {
    if (view) redoCommand(view);
  }

  // reveal puts the cursor at the start of a line and scrolls it into view.
  export function reveal(line: number) {
    if (!view || line < 1 || line > view.state.doc.lines) return;
    const at = view.state.doc.line(line).from;
    view.dispatch({ selection: EditorSelection.cursor(at), effects: EditorView.scrollIntoView(at, { y: "center" }) });
    view.focus();
  }
</script>

<div class="yaml" bind:this={holder}></div>

<style>
  .yaml {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .yaml :global(.cm-editor) {
    flex: 1;
    min-height: var(--editor-height, 420px);
  }
</style>
