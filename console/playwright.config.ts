import { defineConfig } from "@playwright/test";

// The screen tests: the console's build, served by tests/serve.js under the API's own policy, drawn
// in Chromium and measured. They read dist/, so npm run build comes first.
export default defineConfig({
  testDir: "tests/screens",
  testMatch: "*.spec.ts",
  fullyParallel: true,
  timeout: 180_000,
  reporter: process.env.CI ? [["list"], ["github"]] : "list",
  use: { browserName: "chromium" },
});
