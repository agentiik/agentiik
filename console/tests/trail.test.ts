import { describe, expect, it } from "vitest";
import { titleOf } from "../src/lib/trail";

describe("the browser's tab", () => {
  it("names the screen, the nearest first, and the console last", () => {
    expect(titleOf({ kind: "landing" })).toBe("Agentiik");
    expect(titleOf({ kind: "namespace", namespace: "finance", view: "workflows" })).toBe("Workflows · finance · Agentiik");
    expect(titleOf({ kind: "namespace", namespace: "finance", view: "variables" })).toBe("Variables · finance · Agentiik");
    expect(titleOf({ kind: "account", tab: "tokens" })).toBe("API tokens · Your account · Agentiik");
    expect(titleOf({ kind: "runners", tab: "statistics" })).toBe("Statistics · Runners · Agentiik");
  });

  it("leaves out under a workflow what the workflow already says", () => {
    expect(titleOf({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing" })).toBe("monthly-invoicing · finance · Agentiik");
    expect(titleOf({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing", tab: "runs" })).toBe("Runs · monthly-invoicing · finance · Agentiik");
    expect(titleOf({ kind: "namespace", namespace: "finance", view: "workflows", workflow: "monthly-invoicing", run: "01JMZ8W4K2R7QX6T1N3P5V7Y9A" })).toBe("01JMZ8W4K2R7QX6T1N3P5V7Y9A · monthly-invoicing · finance · Agentiik");
  });
});
