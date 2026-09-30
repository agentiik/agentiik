# The web console

The console the API serves at the root of the public URL, written in TypeScript with Svelte 5 and built with Vite into `dist/`, which package `console` embeds into `agentiik-api`. The documentation says what it shows and why it is served from the API's binary: <https://agentiik.github.io/docs#ui>.

## Building and running it

```sh
npm ci
npm run build                    # into dist/, which agentiik-api then carries
npm run check                    # svelte-check, types and components, warnings included
npm test                         # vitest, on recorded answers of the API
AGK_CONSOLE_API=https://localhost:8443 npm run dev   # against a running installation
node tests/serve.js tests/fixtures/alice.json 4173   # against a recorded scenario, with the API's own headers
```

`AGK_VERSION` names the release a build is part of, which the key line shows; a build nobody named says `(devel)`, as `agk` does.

## What it is held to

- **One dialect with the API.** `src/api/schema.d.ts` is generated from `vendor/openapi.json` by `npm run generate`, and the client is typed from it, so a route `openapi.json` does not describe does not compile. The workflow regenerates it and fails where the file committed differs.
- **The API's Content-Security-Policy.** The console loads its own scripts, styles, fonts and icons and reaches its own origin, nothing else: no style or script is written into the page, and nothing is inlined as a `data:` address. `tests/serve.js` reads the policy out of `api/console.go` and serves the build under it.
- **Its addresses.** The build's addresses are relative, `base: "./"`, since the API rewrites the page's `<base>` to the public URL's path.
- **Hide, never disable.** What the principal does not hold, as `GET /api/v1/me` gives it, is left out rather than drawn disabled, and what it may not see is answered as what does not exist.

## Dependencies

`package.json` holds no comment, so each dependency's reason is written here. Every one is under a permissive licence.

Shipped in the build:

| Package | Why |
| --- | --- |
| `svelte` | The framework, compiled into the code that updates each component, so the browser loads a small runtime rather than a library. The site records the choice and its reasons (#ui). MIT. |
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
