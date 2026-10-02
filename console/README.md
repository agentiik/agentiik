# The web console

The console the API serves at the root of the public URL, written in TypeScript with Svelte 5 and built with Vite into `dist/`, which package `console` embeds into `agentiik-api`. The documentation says what it shows and why it is served from the API's binary: <https://agentiik.github.io/docs#ui>.

## Building and running it

```sh
npm ci
npm run build                    # into dist/, which agentiik-api then carries
npm run check                    # svelte-check, types and components, warnings included
npm test                         # vitest, on recorded answers of the API
npm run screens                  # Playwright: every screen drawn and measured, after npm run build
AGK_CONSOLE_API=https://localhost:8443 npm run dev   # against a running installation
node tests/serve.js tests/fixtures/alice.json 4173   # against a recorded scenario, with the API's own headers
```

`AGK_VERSION` names the release a build is part of, which the key line shows; a build nobody named says `(devel)`, as `agk` does.

## What it is held to

- **One dialect with the API.** `src/api/schema.d.ts` is generated from `vendor/openapi.json` by `npm run generate`, and the client is typed from it, so a route `openapi.json` does not describe does not compile. The workflow regenerates it and fails where the file committed differs.
- **The API's Content-Security-Policy.** The console loads its own scripts, styles, fonts and icons and reaches its own origin, nothing else: no style or script is written into the page, and nothing is inlined as a `data:` address. CodeMirror's styles, which its `style-mod` writes into a document as a style element, are instead a stylesheet built in script that the document adopts, which the policy lets through (`src/lib/editor/adopted-styles.ts`). `tests/serve.js` reads the policy out of `api/console.go` and serves the build under it, and `npm run screens` fails on anything it refuses.
- **Its addresses.** The build's addresses are relative, `base: "./"`, since the API rewrites the page's `<base>` to the public URL's path.
- **Aligned to the pixel.** Every line is a whole number of units high, two pixels, so that every box starts on a whole pixel and every line centred in a bar, a row or a control, all an even number of pixels high, does too; every button, field and choice is `--control-height` high; a hairline across a bar, a header or a table row is drawn inside its box, as an inset shadow, so that what is centred in it is centred in its whole height. `npm run screens` draws every screen the recorded scenarios reach at 2560, 1440, 1099, 759 and 390 pixels in both themes, each page read down to its end first as a person scrolls it, since the editor lays out its lines as they come into view, and fails on anything out of line: a box off a whole pixel, two controls of different heights, a row's centres or baselines apart, a pane's content not starting under its title, anything running off the page.
- **No explanation on screen.** A screen says what it shows with labels, values and short empty states, never a sentence explaining a feature: one that needs explaining is built wrong. A failure is told by `lib/problem.ts` and drawn by `Problem` or a notice: what could not be done, the reason in a few words, decided by the status, and the server's answer in small type.
- **A button's words never change.** No name, count or word chosen by a state in a control's label, since its width would change with the value and move what is beside it: what it acts on is said by its row, its pane or the question before it. A confirmation puts Keep before the same button turned red, so that the button clicked stays where it was. `tests/labels.test.ts` reads every component for a control whose label holds an expression.
- **Hide, never disable.** What the principal does not hold, as `GET /api/v1/me` gives it, is left out rather than drawn disabled, and what it may not see is answered as what does not exist.

## Dependencies

`package.json` holds no comment, so each dependency's reason is written here. Every one is under a permissive licence.

Shipped in the build:

| Package | Why |
| --- | --- |
| `svelte` | The framework, compiled into the code that updates each component, so the browser loads a small runtime rather than a library. The site records the choice and its reasons (#ui). MIT. |
| `uplot` | The charts, 51 kB and no dependency of its own, drawing on a canvas with zoom by dragging and a readout kept in step across a page's charts. The site records the choice and the others weighed (#console-statistics). MIT. |
| `openapi-fetch` | The client, typed from the paths `openapi-typescript` generates, a thin layer over `fetch` that adds nothing of its own at run time. MIT. |
| `@codemirror/view`, `@codemirror/state`, `@codemirror/commands`, `@codemirror/language`, `@codemirror/lang-yaml`, `@codemirror/autocomplete`, `@codemirror/lint`, `@codemirror/search`, `@lezer/highlight` | The editor of `agentiik.yaml`, CodeMirror 6, taken module by module so that the build carries what the editor uses and the page that never opens it loads none of it: the editor is a chunk of its own, imported when it is opened. A text area has no colouring, completion or line marks, and a larger editor would be several times its size for features a workflow file does not need. The site records the choice and the others weighed (#ui). MIT. |
| `style-mod` | What CodeMirror draws its styles with, already among its dependencies and named here because the console mounts them as a stylesheet the document adopts, the one way `style-src 'self'` lets them through (`src/lib/editor/adopted-styles.ts`). MIT. |
| `@fontsource-variable/archivo`, `@fontsource-variable/jetbrains-mono` | The design system's two faces, as variable fonts served from the console's own files, since a font service would learn who opens the console and when. Only the Latin subsets are loaded. SIL Open Font License 1.1. |

Used to build and test it, never shipped:

| Package | Why |
| --- | --- |
| `vite`, `@sveltejs/vite-plugin-svelte` | The build, which writes styles into files of their own, as `style-src 'self'` requires. MIT. |
| `typescript`, `svelte-check` | Types, and the components' types and warnings. Apache-2.0 and MIT. |
| `openapi-typescript` | Generates the paths of `openapi.json` as types. MIT. |
| `vitest`, `jsdom`, `@testing-library/svelte` | The tests, the components drawn in a document with no browser. MIT. |
| `@playwright/test` | The screen tests, in a real browser under the API's policy, at the version whose Chromium the development container carries. Apache-2.0. |

## What is copied here

`vendor/` holds what other repositories of the project publish, copied rather than fetched so that a build needs no network and a change upstream is a commit here that a person reads. `vendor/README.md` says where each file comes from.
