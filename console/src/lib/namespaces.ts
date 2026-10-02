import { refusal, type API, type Namespace } from "../api/client";

// pictureOf is where a namespace's picture is read, an address that changes each time it is set, so
// that a browser keeping the old one for a day asks for the new one at once; none where it has none.
export function pictureOf(namespace: Pick<Namespace, "name" | "avatar_updated_at"> | undefined): string | undefined {
  const at = namespace?.avatar_updated_at;
  return namespace && at ? `api/v1/namespaces/${encodeURIComponent(namespace.name)}/avatar?v=${encodeURIComponent(at)}` : undefined;
}

export async function setPicture(api: API, ns: string, file: Blob): Promise<void> {
  const answer = await api.PUT("/api/v1/namespaces/{ns}/avatar", {
    params: { path: { ns } },
    // The body is the file's bytes as they are, which openapi-fetch would otherwise write as JSON.
    body: file as unknown as string,
    bodySerializer: (b: unknown) => b as BodyInit,
    headers: { "Content-Type": file.type },
  });
  if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
}

export async function removePicture(api: API, ns: string): Promise<void> {
  const answer = await api.DELETE("/api/v1/namespaces/{ns}/avatar", { params: { path: { ns } } });
  if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
}
