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
- **The API's Content-Security-Policy.** The console loads its own scripts, styles, fonts and icons and reaches its own origin, nothing else: no style or script is written into the page, and nothing is inlined as a `data:` address. `tests/serve.js` reads the policy out of `api/console.go` and serves the build under it.
- **Its addresses.** The build's addresses are relative, `base: "./"`, since the API rewrites the page's `<base>` to the public URL's path.
- **Aligned to the pixel.** Every line is a whole number of units high, two pixels, so that every box starts on a whole pixel and every line centred in a bar, a row or a control, all an even number of pixels high, does too; every button, field and choice is `--control-height` high; a hairline across a bar, a header or a table row is drawn inside its box, as an inset shadow, so that what is centred in it is centred in its whole height. `npm run screens` draws every screen the recorded scenarios reach at 1440, 1099, 759 and 390 pixels in both themes and fails on anything out of line: a box off a whole pixel, two controls of different heights, a row's centres or baselines apart, a pane's content not starting under its title, anything running off the page.
- **Say what happened, why and what to do.** A failure is told by `lib/problem.ts` and drawn by `Problem` or a notice: what could not be done, as "Could not …", then the reason in everyday words, decided by the status rather than by the API's sentence, then what the reader can do, and the server's answer last, in small type. A hint or a confirmation is written for someone who has not read the API: it says what a thing is for or what an act did, and never leaves the reader to guess whether to wait, retry or ask someone.
- **Hide, never disable.** What the principal does not hold, as `GET /api/v1/me` gives it, is left out rather than drawn disabled, and what it may not see is answered as what does not exist.

## Dependencies

`package.json` holds no comment, so each dependency's reason is written here. Every one is under a permissive licence.

Shipped in the build:

| Package | Why |
| --- | --- |
| `svelte` | The framework, compiled into the code that updates each component, so the browser loads a small runtime rather than a library. The site records the choice and its reasons (#ui). MIT. |
| `uplot` | The charts, 51 kB and no dependency of its own, drawing on a canvas with zoom by dragging and a readout kept in step across a page's charts. The site records the choice and the others weighed (#console-statistics). MIT. |
| `openapi-fetch` | The client, typed from the paths `openapi-typescript` generates, a thin layer over `fetch` that adds nothing of its own at run time. MIT. |
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
