import { Refusal } from "../api/client";

// A failure as the console tells it: what could not be done, why in everyday words, what the reader
// can do about it, and the server's own answer, its status and its sentence, kept as the detail for
// whoever has to look further. The API's sentences are written for whoever reads the API, "what the
// caller holds could not be read", and shown alone they leave a person not knowing whether they did
// something wrong, whether to wait or whom to ask; so the console says that first, from the status,
// and quotes the sentence after it, unchanged.

export type Explained = {
  // what could not be done, as a sentence: "Could not load the runs."
  what: string;
  // why, in everyday words.
  why: string;
  // what the reader can do, where something can be done.
  next: string;
  // the server's answer as it gave it, or what the browser said where nothing came back.
  detail: string;
  // whether trying again may succeed, which a retry button is offered for.
  transient: boolean;
};

// Told is a failure whose message is already its reason for a person, a passkey request the browser
// ended or a browser that offers none: explain gives it as the reason rather than as a request that
// did not reach Agentiik.
export class Told extends Error {}

// explain tells a failure to do something, the thing written as an infinitive: "load the runs",
// "save the profile". The cause is a Refusal the API answered, or whatever fetch threw where no
// answer came back.
export function explain(failed: string, cause: unknown): Explained {
  const what = `Could not ${failed}.`;
  if (cause instanceof Told) return refused(failed, cause.message);
  if (!(cause instanceof Refusal)) {
    const said = cause instanceof Error ? cause.message : String(cause);
    return {
      what,
      why: "Agentiik is not reachable.",
      next: "Check your connection and try again.",
      detail: said,
      transient: true,
    };
  }
  const detail = `${cause.status}${cause.message ? ` ${quoted(cause.message)}` : ""}`;
  const told = (why: string, next: string, transient = false): Explained => ({ what, why, next, detail, transient });
  // Where the server's own sentence is the reason, it is the reason, and the detail keeps the status.
  const toldBy = (why: string, next: string): Explained => ({ what, why, next, detail: String(cause.status), transient: false });
  switch (true) {
    case cause.status === 401:
      return told("Your session has ended.", "Sign in again.");
    // The server's own sentence where it gives one: a 403 is a missing permission, but also a
    // request from another address than the installation's public URL, which no permission
    // changes and which "You do not have permission" would send a person looking for one.
    case cause.status === 403 && cause.message.trim() !== "":
      return toldBy(sentence(cause.message), "");
    case cause.status === 403:
      return told("You do not have permission.", "");
    case cause.status === 404:
      return told("Not found, or not shared with you.", "");
    case cause.status === 409:
      return toldBy(sentence(cause.message), "");
    case cause.status === 413:
      return toldBy(`It is too large: ${lowered(cause.message)}`, "");
    case cause.status === 429:
      return told("Too many requests.", "Wait a moment and try again.", true);
    case cause.status >= 500:
      return told("Server error.", "Try again. If it keeps failing, tell your administrator.", true);
    case cause.status === 400 || cause.status === 422:
      return toldBy(sentence(cause.message), "");
    default:
      return toldBy(sentence(cause.message), "");
  }
}

// refused tells something the console itself refuses before asking the server, a file of the wrong
// kind or a date in the past: what could not be done, and why, which says what to change.
export function refused(failed: string, why: string): Explained {
  return { what: `Could not ${failed}.`, why, next: "", detail: "", transient: false };
}

// sentence is one of the API's sentences as a sentence of the console's: a capital and a full stop.
function sentence(text: string): string {
  const t = text.trim();
  if (!t) return "The server refused it.";
  const capital = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(capital) ? capital : `${capital}.`;
}

// lowered is one of the API's sentences carried on after a colon, ending with a full stop.
function lowered(text: string): string {
  const t = text.trim();
  return /[.!?]$/.test(t) ? t : `${t}.`;
}

function quoted(text: string): string {
  return `“${text.trim()}”`;
}
