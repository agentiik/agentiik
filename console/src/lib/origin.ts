// Where the console works: the pages of the installation's public URL. A passkey ceremony, a password
// sign-in, every change a session makes and the live connection are taken from that origin alone, so
// a console opened at another, an IP address where the public URL names a host or a host the proxy in
// front also answers, draws every screen and can sign nobody in. The API writes the public URL's
// origin into the page beside its <base>, and the console says so at once, naming the address that
// works, rather than leave its reader with a refusal at every click.

// Opened is what the console needs of where it was opened: the page's address, and what the API wrote
// into it.
export interface Opened {
  readonly href: string;
  readonly origin: string;
  readonly declared: string | null;
}

// elsewhere is the same page at the public URL's origin when the console was opened at another, and
// null when it was opened where it works or the page names no origin, as the development server's
// does, since nothing is then known to be wrong.
export function elsewhere(opened: Opened): string | null {
  const declared = opened.declared?.trim();
  if (!declared || declared === opened.origin) return null;
  try {
    const here = new URL(opened.href);
    const works = new URL(declared);
    if (works.origin === opened.origin) return null;
    // Built from the origin rather than by changing the host, which would keep the port the console
    // was opened at where the public URL names none.
    return new URL(here.pathname + here.search + here.hash, works.origin).href;
  } catch {
    return null;
  }
}

// openedIn reads where the console was opened from the browser's document.
export function openedIn(doc: Document, location: Location): Opened {
  return { href: location.href, origin: location.origin, declared: doc.querySelector('meta[name="agentiik-origin"]')?.getAttribute("content") ?? null };
}
