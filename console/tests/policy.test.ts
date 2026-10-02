import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import { connect } from "../src/api/client";
import App from "../src/App.svelte";
import { Place } from "../src/lib/place.svelte";
import { Session } from "../src/lib/session.svelte";
import { scenario, type Scenario } from "./scenario";

// An installation answering from a scenario, keeping each request that writes with its body.
function open(path: string, s: Scenario) {
  const asked: { key: string; body: unknown }[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const key = `${request.method} ${url.pathname}`;
    if (request.method !== "GET") {
      const text = await request.text();
      asked.push({ key, body: text ? JSON.parse(text) : undefined });
    }
    const recorded = s[`${key}${url.search}`] ?? s[key] ?? { status: 404, body: { error: "no such thing, or not yours" } };
    return new Response(recorded.body === undefined ? null : JSON.stringify(recorded.body), { status: recorded.status, headers: { "Content-Type": "application/json" } });
  };
  const api = connect("http://stand-in/", fetcher);
  const [pathname, search] = path.split("?");
  const place = new Place({ pathname: pathname!, search: search ? `?${search}` : "", baseURI: "http://stand-in/" }, { pushState() {}, replaceState() {} });
  render(App, { api, session: new Session(api), place, version: "v0.6.0", passkeys: { unavailable: "" } });
  return { asked, place };
}

const policy = { password: "allowed", passkey: "required", user_verification: "required", device_bound_only: false, min_passkeys: 2 };

describe("the installation's sign-in policy", () => {
  it("is a tab beside the users, each setting by its own name, and sends every setting when one changes", async () => {
    const s = scenario("dana");
    s["PUT /api/v1/auth/policy"] = { status: 200, body: { ...policy, min_passkeys: 3 } };
    const { asked } = open("/users/policy", s);
    const nav = await screen.findByRole("navigation", { name: "Users, what is shown" });
    expect(within(nav).getByRole("link", { name: "Sign-in policy" }).getAttribute("aria-current")).toBe("page");
    const form = await screen.findByRole("form", { name: "The installation's sign-in policy" });
    expect((within(form).getByRole("combobox", { name: "password" }) as HTMLSelectElement).value).toBe("allowed");
    const save = within(form).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    await fireEvent.input(within(form).getByRole("spinbutton", { name: "min_passkeys" }), { target: { value: "3" } });
    await fireEvent.submit(form);
    expect(await screen.findByText("Policy saved.")).toBeTruthy();
    expect(asked).toEqual([{ key: "PUT /api/v1/auth/policy", body: { ...policy, min_passkeys: 3 } }]);
  });

  it("asks again before passwords are forbidden, and keeps them on Keep", async () => {
    const s = scenario("dana");
    s["PUT /api/v1/auth/policy"] = { status: 200, body: { ...policy, password: "forbidden" } };
    const { asked } = open("/users/policy", s);
    const form = await screen.findByRole("form", { name: "The installation's sign-in policy" });
    await fireEvent.change(within(form).getByRole("combobox", { name: "password" }), { target: { value: "forbidden" } });
    await fireEvent.submit(form);
    expect(within(form).getByRole("alert").textContent).toMatch(/Every stored password and one-time code generator is deleted/);
    expect(asked).toEqual([]);
    await fireEvent.click(within(form).getByRole("button", { name: "Keep" }));
    expect((within(form).getByRole("combobox", { name: "password" }) as HTMLSelectElement).value).toBe("allowed");
    await fireEvent.change(within(form).getByRole("combobox", { name: "password" }), { target: { value: "forbidden" } });
    await fireEvent.submit(form);
    await fireEvent.submit(form);
    expect(await screen.findByText("Policy saved.")).toBeTruthy();
    expect(asked).toEqual([{ key: "PUT /api/v1/auth/policy", body: { ...policy, password: "forbidden" } }]);
  });

  it("is no page of a caller who administers nothing", async () => {
    open("/users/policy", scenario("alice"));
    expect((await screen.findAllByText("This page does not exist, or is not shared with you.")).length).toBeGreaterThan(0);
  });
});

describe("a namespace's sign-in policy", () => {
  it("is read by its members, each setting the namespace's or the installation's", async () => {
    open("/finance/settings", scenario("alice"));
    const pane = await screen.findByRole("region", { name: "Sign-in policy" });
    expect(await within(pane).findByText("allowed, as the installation")).toBeTruthy();
    expect(within(pane).getByText("true")).toBeTruthy();
    expect(within(pane).queryByRole("form")).toBeNull();
  });

  it("is tightened by an administrator, offered only what is stricter than the installation and sending what it sets", async () => {
    const s = scenario("dana");
    s["PUT /api/v1/finance/auth/policy"] = { status: 200, body: { device_bound_only: true, password: "forbidden" } };
    const { asked } = open("/namespaces?namespace=finance", s);
    const form = await screen.findByRole("form", { name: "The namespace's sign-in policy" });
    expect(within(within(form).getByRole("combobox", { name: "passkey" })).getAllByRole("option").map((o) => o.textContent)).toEqual(["required, as the installation"]);
    await fireEvent.change(within(form).getByRole("combobox", { name: "password" }), { target: { value: "forbidden" } });
    await fireEvent.submit(form);
    expect(await within(form).findByText("Policy saved.")).toBeTruthy();
    await waitFor(() => expect(asked).toEqual([{ key: "PUT /api/v1/finance/auth/policy", body: { password: "forbidden", device_bound_only: true } }]));
  });
});
