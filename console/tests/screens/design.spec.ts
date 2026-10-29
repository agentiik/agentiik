import { expect, test, type Page } from "@playwright/test";
import { scenarioNamed, serve } from "../serve.js";

// The design system's rules that only a drawn screen can be held to, its styles worked out by the
// browser from the tokens and the cascade: each test opens the screens that draw what a rule is about,
// at a wide window in the light theme, and compares what is computed with the token the rule names.

const run = "01JMZ8W4K2R7QX6T1N3P5V7Y9A";
const scenario = scenarioNamed("alice");
// A step's log, one line long, so that the log pane draws its tools, Find among them.
const task = `${run}/normalize/1`;
scenario[`GET /api/v1/runs/${run}/steps/normalize/logs`] = {
  status: 200,
  type: "text/event-stream",
  text: [
    ["dispatch", { task_id: "t-1", idempotency_key: task, attempt: 1, requeue: 0 }],
    ["line", { task_id: "t-1", line: 1, at: "2026-09-30T06:00:12Z", text: "normalized 42 orders" }],
    ["dispatch_end", { task_id: "t-1", lines: 1, truncated: false, final: true }],
    ["end", { verdict: "succeeded" }],
  ]
    .map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
    .join(""),
};

let server: Awaited<ReturnType<typeof serve>>;
test.beforeAll(async () => {
  server = await serve({ scenario, prefix: "/" });
});
test.afterAll(async () => {
  await server.close();
});

async function opened(page: Page, path: string) {
  await page.clock.install({ time: new Date("2026-10-01T06:02:30Z") });
  await page.goto(server.url.replace(/\/$/, "") + path);
  await page.locator("main").first().waitFor();
  await page.evaluate(() => document.fonts.ready);
}

// token is what a colour token comes to in the page, as the browser computes it on a property.
function token(page: Page, name: string, property: "color" | "background-color" | "border-top-color"): Promise<string> {
  return page.evaluate(
    ([name, property]) => {
      const probe = document.createElement("span");
      probe.style.setProperty(property!, `var(--${name})`);
      document.body.append(probe);
      const value = getComputedStyle(probe).getPropertyValue(property!);
      probe.remove();
      return value;
    },
    [name, property],
  );
}

test.describe("the design system, as drawn", () => {
  test.use({ viewport: { width: 1440, height: 900 }, colorScheme: "light", deviceScaleFactor: 1, locale: "en-GB", timezoneId: "UTC" });

  // "2.5px for the element that carries the current focus": a field, and the box a filter or the
  // log's Find draws around its field, which is the field a person sees. A browser computes a width
  // in whole pixels, 2.5px as 2, so a field's is held to what border.focus comes to drawn the same way.
  test("a field in focus is bordered at 2.5px in the accent", async ({ page }) => {
    const fields = [
      { path: "/finance/workflows/monthly-invoicing/files", focused: "#files-ref", bordered: "#files-ref" },
      { path: "/finance/workflows", focused: "label.filter input", bordered: "label.filter" },
      { path: `/finance/workflows/monthly-invoicing/runs/${run}?step=normalize&pane=logs`, focused: "label.find input", bordered: "label.find" },
    ];
    const found: string[] = [];
    for (const f of fields) {
      await opened(page, f.path);
      await page.locator(f.focused).focus();
      const accent = await token(page, "accent", "color");
      const focus = await page.evaluate(() => {
        const probe = document.createElement("span");
        probe.style.setProperty("outline", "var(--border-focus) solid");
        document.body.append(probe);
        const width = getComputedStyle(probe).outlineWidth;
        probe.remove();
        return width;
      });
      const outline = await page.locator(f.bordered).evaluate((el) => {
        const s = getComputedStyle(el);
        return { width: s.outlineWidth, style: s.outlineStyle, color: s.outlineColor };
      });
      if (outline.width !== focus || outline.style !== "solid" || outline.color !== accent) found.push(`${f.path} ${f.bordered}: ${outline.width} ${outline.style} ${outline.color}, border.focus ${focus} in ${accent}`);
    }
    expect(found).toEqual([]);
  });
});
