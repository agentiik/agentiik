import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

// TypeScript in components, and runes everywhere: the console is written for Svelte 5 alone.
export default {
  preprocess: vitePreprocess(),
  compilerOptions: { runes: true },
};
