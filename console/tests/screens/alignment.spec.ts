import { expect, test } from "@playwright/test";
import { scenarioNamed, serve } from "../serve.js";
import { measure, type Finding } from "./measure";

// Every screen the recorded scenarios reach, drawn in Chromium under the console's own policy at the
// widths its layout changes at, a large screen, a wide window, the last width with the sidebar
// folded, the last with it shown as a drawer and a phone, in both themes, and measured: nothing runs off the page, every
// control is one height and a row of them one band, what is centred in a row is centred on one line
// and text side by side on one baseline, each pane's content starts under its title, and every box
// starts on a whole pixel. A finding names the rule, the element and what was measured.

const screens: Record<string, string[]> = {
  alice: [
    "/",
    "/finance/workflows/monthly-invoicing/runs",
    "/finance/workflows/monthly-invoicing/runs/01JMZ8W4K2R7QX6T1N3P5V7Y9A",
    "/finance/workflows/monthly-invoicing/runs/01JMZ8W4K2R7QX6T1N3P5V7Y9A?step=invoice",
    "/finance/workflows/monthly-invoicing/runs/01JMZ8V1P9C4XQ7K2N4D6F8H0A",
    "/finance/workflows/monthly-invoicing/runs/01JMZ8Q6F1T7QK2N4D6F8H0A2F",
    "/finance/workflows",
    "/finance/workflows/monthly-invoicing",
    "/finance/workflows/monthly-invoicing?step=archive",
    "/finance/workflows/monthly-invoicing/files",
    "/finance/workflows/monthly-invoicing/mcp",
    "/finance/workflows/monthly-invoicing/statistics",
    "/finance/workflows/monthly-invoicing?edit=1&step=invoice",
    "/finance/statistics",
    "/finance/sharing",
    "/finance/variables",
    "/finance/settings",
    "/alice/workflows/report",
    "/alice/workflows/vat-reconciliation/files",
    "/team-ops/workflows/nightly?step=export",
    "/alice/sharing",
    "/alice/variables",
    "/me",
    "/me/profile",
    "/me/tokens",
    "/me/service-accounts",
  ],
  dana: ["/", "/runners", "/runners/statistics", "/users", "/groups", "/namespaces", "/finance/settings", "/me"],
};

const widths = [2560, 1440, 1099, 759, 390];
const themes = ["light", "dark"] as const;

for (const [who, paths] of Object.entries(screens)) {
  test.describe(`${who}'s screens`, () => {
    let server: Awaited<ReturnType<typeof serve>>;
    test.beforeAll(async () => {
      server = await serve({ scenario: scenarioNamed(who), prefix: "/" });
    });
    test.afterAll(async () => {
      await server.close();
    });

    for (const theme of themes) {
      for (const width of widths) {
        test(`line up at ${width}px in the ${theme} theme`, async ({ browser }) => {
          const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: theme, deviceScaleFactor: 1, locale: "en-GB", timezoneId: "UTC" });
          const found: (Finding & { path: string })[] = [];
          for (const path of paths) {
            const page = await context.newPage();
            await page.clock.install({ time: new Date("2026-10-01T06:02:30Z") });
            await page.goto(server.url.replace(/\/$/, "") + path);
            await page.locator("main").first().waitFor();
            await page.evaluate(() => document.fonts.ready);
            // A chart is drawn once its box has been measured, on the frame after.
            await page.waitForTimeout(300);
            for (const f of await page.evaluate(measure)) found.push({ path, ...f });
            await page.close();
          }
          await context.close();
          expect(found).toEqual([]);
        });
      }
    }
  });
}
