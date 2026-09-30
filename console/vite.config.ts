import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vitest/config";

// The build goes into dist/, which package console embeds into agentiik-api. dist/ carries a committed
// .gitignore, since //go:embed refuses a directory it matches nothing in, so the build writes beside it
// and never empties the directory first, as that file says.
const outDir = "dist";

// AGK_CONSOLE_API is an installation the development server sends the API's paths to, so that the
// console is written against a real one; the page and its scripts stay the development server's.
const api = process.env.AGK_CONSOLE_API;

export default defineConfig({
  plugins: [svelte()],
  define: {
    __AGENTIIK_VERSION__: JSON.stringify(process.env.AGK_VERSION || "(devel)"),
  },
  // Relative, because the API rewrites the page's <base> to the public URL's path, which is where
  // every address of the build then resolves from, at any depth and under any path a proxy serves
  // the installation at.
  base: "./",
  build: {
    outDir,
    emptyOutDir: false,
    assetsDir: "assets",
    // Nothing is written into the page or a script as data: the Content-Security-Policy the API
    // sends lets the console load its own files, and a font or an icon inlined as a data: address
    // would be one more kind of source to allow.
    assetsInlineLimit: 0,
  },
  server: api
    ? {
        proxy: Object.fromEntries(
          ["/api", "/auth", "/hooks", "/mcp", "/objects"].map((path) => [path, { target: api, changeOrigin: false, secure: false }]),
        ),
      }
    : undefined,
  resolve: process.env.VITEST ? { conditions: ["browser"] } : undefined,
  test: {
    environment: "jsdom",
    include: ["tests/**/*.test.ts"],
  },
});
