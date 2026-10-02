// What a user says of themself, as PATCH /api/v1/me writes it, and their photo, as
// /api/v1/me/avatar sets and removes it. A profile is the user's alone: no administrator writes it,
// and a service account has none. Their display name is written by nobody: it is made of their given
// and family names, so the form has no field for it.

import { refusal, type API, type Me } from "../api/client";
import type { components } from "../api/schema";

export type User = components["schemas"]["user"];

export const fields = ["given_name", "family_name", "title", "location", "timezone", "bio"] as const;
export type Field = (typeof fields)[number];
export type Profile = Record<Field, string>;

// The longest each is, in characters, as the API holds them, so that the form refuses what the API
// would before anything is sent.
export const longest: Record<Field, number> = { given_name: 128, family_name: 128, title: 128, location: 128, timezone: 64, bio: 280 };

export function profileOf(user: User): Profile {
  return {
    given_name: user.given_name ?? "",
    family_name: user.family_name ?? "",
    title: user.title ?? "",
    location: user.location ?? "",
    timezone: user.timezone ?? "",
    bio: user.bio ?? "",
  };
}

// changed is what the form holds that the profile does not, each field trimmed, which is all
// PATCH /api/v1/me is sent: a field left out is kept as it is.
export function changed(before: Profile, after: Profile): Partial<Profile> {
  const out: Partial<Profile> = {};
  for (const f of fields) {
    const v = after[f].trim();
    if (v !== before[f]) out[f] = v;
  }
  return out;
}

export async function save(api: API, body: Partial<Profile>): Promise<Me> {
  const { data, error, response } = await api.PATCH("/api/v1/me", { body });
  if (!data) throw refusal(response, error);
  return data;
}

// The photos a browser sends, as the API takes them: PUT /api/v1/me/avatar reads a PNG or a JPEG of
// 1 MiB at most, and answers 413 and 415 otherwise, which the form says before sending.
export const photoTypes = ["image/png", "image/jpeg"];
export const photoBytes = 1 << 20;

export async function setPhoto(api: API, file: Blob): Promise<void> {
  const answer = await api.PUT("/api/v1/me/avatar", {
    // The body is the file's bytes as they are, which openapi-fetch would otherwise write as JSON.
    body: file as unknown as string,
    bodySerializer: (b: unknown) => b as BodyInit,
    headers: { "Content-Type": file.type },
  });
  if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
}

export async function removePhoto(api: API): Promise<void> {
  const answer = await api.DELETE("/api/v1/me/avatar");
  if (answer.error !== undefined || !answer.response.ok) throw refusal(answer.response, answer.error);
}

// photoOf is where the caller's photo is read, an address that changes each time it is set, so that
// a browser keeping the old one for a day asks for the new one at once; none where there is no photo.
export function photoOf(me: Me): string | undefined {
  const at = me.user?.avatar_updated_at;
  return at ? `api/v1/me/avatar?v=${encodeURIComponent(at)}` : undefined;
}

// userPhotoOf is a user's photo as an administrator reads it.
export function userPhotoOf(user: User): string | undefined {
  return user.avatar_updated_at ? `api/v1/users/${encodeURIComponent(user.login)}/avatar?v=${encodeURIComponent(user.avatar_updated_at)}` : undefined;
}

// zones are the time zones a browser knows, offered beside the field; the API holds any name the
// IANA database has.
export function zones(): string[] {
  try {
    return (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
}

// localTime is the time it is where somebody is, as their profile's zone has it, or nothing where it
// names none or one this browser does not know.
export function localTime(zone: string, now: number): string | undefined {
  if (!zone) return undefined;
  try {
    return new Date(now).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", timeZone: zone });
  } catch {
    return undefined;
  }
}
