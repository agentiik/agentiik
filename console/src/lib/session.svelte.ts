import { refusal, type API, type Me, type Namespace } from "../api/client";

// Who the console is signed in as, and what it may see, read from GET /api/v1/me and GET
// /api/v1/namespaces, again whenever something may have changed it.
//
// standing is what the console can draw at all: nothing yet; a sign-in, where the API answers 401; a
// session that may only enrol a passkey, which /me refuses with 403 and which the console sends to
// the enrolment page, the one place it may act; or the console itself.

export type Standing = "reading" | "signed-in" | "signed-out" | "enrol-only" | "unreachable";

export class Session {
  standing = $state<Standing>("reading");
  me = $state<Me | null>(null);
  namespaces = $state<Namespace[]>([]);

  // answering is whether the installation answered the console's last read, which the top bar says
  // in words; said is why it did not.
  answering = $state(true);
  said = $state("");

  readonly #api: API;

  constructor(api: API) {
    this.#api = api;
  }

  // read reads who the console is signed in as and the namespaces it sees.
  async read(): Promise<void> {
    let me;
    try {
      me = await this.#api.GET("/api/v1/me");
    } catch (e) {
      this.#unanswered(e instanceof Error ? e.message : String(e));
      return;
    }
    this.answering = true;
    switch (me.response.status) {
      case 401:
        this.me = null;
        this.standing = "signed-out";
        return;
      case 403:
        this.me = null;
        this.standing = "enrol-only";
        return;
    }
    if (!me.data) {
      this.#unanswered(refusal(me.response, me.error).message);
      return;
    }
    let listed;
    try {
      listed = await this.#api.GET("/api/v1/namespaces");
    } catch (e) {
      this.#unanswered(e instanceof Error ? e.message : String(e));
      return;
    }
    this.me = me.data;
    this.namespaces = listed.data?.namespaces ?? [];
    this.standing = "signed-in";
  }

  #unanswered(why: string) {
    this.answering = false;
    this.said = why;
    if (this.standing === "reading") {
      this.standing = "unreachable";
    }
  }

  // dismiss removes one of the caller's notifications, which changes nothing anybody else sees.
  async dismiss(id: string): Promise<void> {
    const { response, error } = await this.#api.DELETE("/api/v1/me/notifications/{id}", { params: { path: { id } } });
    if (!response.ok) {
      throw refusal(response, error);
    }
    if (this.me) {
      this.me.notifications = this.me.notifications.filter((n) => n.id !== id);
    }
  }

  // signOut ends the session this browser holds.
  async signOut(): Promise<void> {
    const { response, error } = await this.#api.POST("/api/v1/auth/sign-out");
    if (!response.ok && response.status !== 401) {
      throw refusal(response, error);
    }
    this.me = null;
    this.namespaces = [];
    this.standing = "signed-out";
  }
}
