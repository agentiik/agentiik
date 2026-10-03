import { refusal, type API } from "../api/client";
import type { components } from "../api/schema";

// The caller's collections: "a connector of a principal's own: the workflows they choose, each one
// a tool, served at /mcp/collections/{id}, which is the URL a client is given". Each call is the
// route under /api/v1/me/collections, which decides everything: what a member offers, a name
// another collection or another member holds, the bounds.

export type Collection = components["schemas"]["collection"];
export type Member = components["schemas"]["collectionMember"];

export async function collectionsOf(api: API): Promise<Collection[]> {
  const { data, error, response } = await api.GET("/api/v1/me/collections");
  if (!data) throw refusal(response, error);
  return data.collections;
}

export async function makeCollection(api: API, name: string, description: string): Promise<Collection> {
  const { data, error, response } = await api.POST("/api/v1/me/collections", { body: description ? { name, description } : { name } });
  if (!data) throw refusal(response, error);
  return data;
}

// changeCollection sets what changed of a collection's name and description, and nothing else.
export async function changeCollection(api: API, id: string, change: { name?: string; description?: string }): Promise<Collection> {
  const { data, error, response } = await api.PATCH("/api/v1/me/collections/{id}", { params: { path: { id } }, body: change });
  if (!data) throw refusal(response, error);
  return data;
}

export async function removeCollection(api: API, id: string): Promise<void> {
  const { error, response } = await api.DELETE("/api/v1/me/collections/{id}", { params: { path: { id } } });
  if (response.status !== 204) throw refusal(response, error);
}

// addMember writes a member whole: its ref and the name its tool goes by, each left out for its
// default, the workflow's default branch and the name its mcp block gives.
export async function addMember(api: API, id: string, workflow: string, member: { ref?: string; as?: string } = {}): Promise<Collection> {
  const [ns, name] = split(workflow);
  const body: { ref?: string; as?: string } = {};
  if (member.ref) body.ref = member.ref;
  if (member.as) body.as = member.as;
  const { data, error, response } = await api.PUT("/api/v1/me/collections/{id}/members/{ns}/{name}", { params: { path: { id, ns, name } }, body });
  if (!data) throw refusal(response, error);
  return data;
}

export async function removeMember(api: API, id: string, workflow: string): Promise<Collection> {
  const [ns, name] = split(workflow);
  const { data, error, response } = await api.DELETE("/api/v1/me/collections/{id}/members/{ns}/{name}", { params: { path: { id, ns, name } } });
  if (!data) throw refusal(response, error);
  return data;
}

function split(workflow: string): [string, string] {
  const at = workflow.indexOf("/");
  return [workflow.slice(0, at), workflow.slice(at + 1)];
}

// offered says what a member offers in a few words: the tool it is offered as, or why it offers
// none, as the API reasons it.
export function offered(m: Member): string {
  if (m.tool) return m.tool;
  return m.reason === "not_runnable" ? "Not runnable" : "No mcp block";
}

// serverAddress is the user's server, /mcp on the installation's public URL, which the page's base
// names: the one address every principal gives a client, each answered as themselves.
export function serverAddress(baseURI: string): string {
  return new URL("mcp", baseURI).href;
}

// mcpServed says whether the installation serves MCP, which the API writes into the page where it
// does not: "the web console's MCP panel says that this one serves none, and collections are kept,
// made and changed as before, to be served once the installation does".
export function mcpServed(doc: Document): boolean {
  return doc.querySelector('meta[name="agentiik-mcp"]')?.getAttribute("content") !== "off";
}
