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

  // A faint state, queued, skipped or cancelled: "muted on sunken", its dot hollow, as
  // design/specimen.html draws it for every client to check itself against.
  test("a faint state's pill is muted on sunken with a hollow dot", async ({ page }) => {
    await opened(page, "/finance/workflows/monthly-invoicing/runs?span=all");
    const sunken = await token(page, "sunken", "background-color");
    const muted = await token(page, "muted", "color");
    const pills = await page.locator(".pill").evaluateAll((all) =>
      all
        .filter((p) => ["queued", "skipped", "cancelled"].includes(p.textContent?.trim() ?? ""))
        .map((p) => {
          const dot = p.querySelector(".dot");
          return { state: p.textContent?.trim(), ground: getComputedStyle(p).backgroundColor, ink: getComputedStyle(p).color, dot: dot ? getComputedStyle(dot).backgroundColor : "" };
        }),
    );
    expect(pills.map((p) => p.state)).toEqual(expect.arrayContaining(["queued", "cancelled"]));
    expect(pills.filter((p) => p.ground !== sunken || p.ink !== muted || p.dot !== "rgba(0, 0, 0, 0)")).toEqual([]);
  });
});

// Opened at an address other than the public URL, the console says so on a line at the top of the
// window, and the sidebar stands below it: "the sidebar stays in the window" as the page scrolls,
// its foot included, and on a phone the top bar and the drawer stand below it too.
test.describe("a console opened elsewhere", () => {
  let elsewhere: Awaited<ReturnType<typeof serve>>;
  test.beforeAll(async () => {
    elsewhere = await serve({ scenario, prefix: "/", origin: "https://agentiik.example.com" });
  });
  test.afterAll(async () => {
    await elsewhere.close();
  });

  // placed is where the line, the sidebar, its last entry and a phone's top bar are drawn.
  const placed = (page: Page) =>
    page.evaluate(() => {
      const bottom = (selector: string) => document.querySelector(selector)?.getBoundingClientRect();
      return {
        line: bottom(".elsewhere")!.bottom,
        side: bottom(".side")!,
        last: bottom(".sidebar > :last-child")!.bottom,
        bar: bottom("header.bar")?.top,
        window: window.innerHeight,
      };
    });

  for (const width of [1440, 1099]) {
    test(`keeps the sidebar's foot in the window at ${width}px`, async ({ browser }) => {
      const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: "light", deviceScaleFactor: 1, locale: "en-GB", timezoneId: "UTC" });
      const found: string[] = [];
      for (const path of ["/", "/alice/variables"]) {
        const page = await context.newPage();
        await page.clock.install({ time: new Date("2026-10-01T06:02:30Z") });
        await page.goto(elsewhere.url.replace(/\/$/, "") + path);
        await page.locator(".sidebar").waitFor();
        await page.waitForFunction(() => getComputedStyle(document.querySelector(".frame")!).getPropertyValue("--elsewhere") !== "0px");
        for (const at of ["top", "end"]) {
          if (at === "end") await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
          const p = await placed(page);
          if (p.side.top < p.line - 0.5) found.push(`${path} at its ${at}: the sidebar starts at ${p.side.top}, under the line ending at ${p.line}`);
          if (p.side.bottom > p.window + 0.5 || p.last > p.window + 0.5) found.push(`${path} at its ${at}: the sidebar ends at ${p.side.bottom}, its last entry at ${p.last}, the window at ${p.window}`);
        }
        await page.close();
      }
      await context.close();
      expect(found).toEqual([]);
    });
  }

  test("keeps a phone's top bar and drawer below the line", async ({ browser }) => {
    const context = await browser.newContext({ viewport: { width: 390, height: 844 }, colorScheme: "light", deviceScaleFactor: 1, locale: "en-GB", timezoneId: "UTC" });
    const page = await context.newPage();
    await page.clock.install({ time: new Date("2026-10-01T06:02:30Z") });
    await page.goto(elsewhere.url.replace(/\/$/, "") + "/");
    await page.locator("header.bar").waitFor();
    await page.waitForFunction(() => getComputedStyle(document.querySelector(".frame")!).getPropertyValue("--elsewhere") !== "0px");
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const scrolled = await placed(page);
    expect(scrolled.bar).toBeGreaterThanOrEqual(scrolled.line - 0.5);
    await page.locator("header.bar button.menu-button").click();
    const drawn = await placed(page);
    expect(drawn.side.top).toBeGreaterThanOrEqual(drawn.line - 0.5);
    expect(drawn.side.bottom).toBeLessThanOrEqual(drawn.window + 0.5);
    await context.close();
  });
});
