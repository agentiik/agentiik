# Changelog

The releases of `agentiik`. Every repository carries the same version and is tagged at the same moment, so an entry may say that nothing changed; [Versioning](https://agentiik.github.io/docs#versioning) says why. `0.y.z` promises nothing beyond itself.

## Unreleased

### Upgrading

- A version stored before this release naming a port or a workflow output of 251 to 255 characters, or a secret its namespace does not declare, rebuilds, starts and runs as before, with nothing to do: both rules are applied where a version is made, and never where a stored one is read back (`version.Build`, `graph.LoadStored`, `brick.ParseStoredManifest`, a task's read of its image, `agk run --namespace`). Pushed again with the same files, from an agk of any release, it is answered as that version, unchanged; with other files, 409 as before.
- A version stored before this release from a directory of its repository, under a name its `metadata` does not write, or relocating a file to a relative path, rebuilds, starts and runs as before, with nothing to do, and the push route answers it as that version when an earlier agk pushes it again: those rules are applied where a version is made (`version.Check`), which is also why this release's `agk push` makes none.
- A workflow v0.2 or v0.3 pushed as trees becomes an empty repository at the upgrade, with nothing to do: migration 0049 gives it a key of its own for its packs and its default branch unborn and unprotected, so that an editor's `agk push` keeps landing, and keeps every version, run and counted object as it was, each version recorded as sent as a tree (`workflow_versions.source`).
- A version recorded before this release runs as it ran, with nothing to do: its tasks are handed its tree whole, whatever its steps' `files` select, and a file their long form relocates is there as well, with the tree's mode rather than the one asked for, which was not applied then. One recorded from this release on, a stored commit pushed again excepted, is handed what its `files` select; the mark is `selects_files` in the version's stored bundle, which one recorded before carries none of.

### Workflows

- One validation, `version.Check`, judges a tree for `agk validate`, `agk run --local`, `agk graph`, `agk push` and the push route, reaching the namespace's pins, manifests and secret declarations, the repositories a workflow include names and the pusher's `secret:use` through resolvers each caller gives.
- A refusal by a rule names the file, line and column of the value refused, in whichever file wrote it, and a rule spelled as the repository fixtures spell it, from `include-leaves-tree` to `image-not-pinned`, with one rule per key an included file may not carry and `dot-git-in-tree`, `backslash-in-tree`, `tree-path-too-long` and `tree-name-too-long` for the tree rules moved out of the API.
- A step keeps its whole `extends` chain (`graph.Step.Extends`), a workflow include reads the other repository's root `agentiik.yaml` as a fragment with its path includes resolved there (`graph.Remote`), and the includes are recorded in the order they applied.
- A version's graph is written as `wire.schema.json#/$defs/resolvedGraph` (`graph.Graph.Resolved`), leaving out keywords at the language's default, the script keywords of a brick or a call, and the namespace.
- A file relocated to a relative path by `files` is refused where a version is made.

### API

- A push naming a port or a workflow output past 250 characters (`agk.PortMaxBytes`), in the entry point, a file it includes or a manifest it carries, is refused with 422 before any of its tree is stored (`version.Check`).
- A workflow its first push creates is a repository whose default branch is unborn and unprotected, as every repository is until an owner protects it.
- A push naming a port or a workflow output past 250 characters (`agk.PortMaxBytes`), in the entry point, a file it includes or a manifest it carries, is refused with 422 before any of its tree is stored (`version.BuildNew`).
- A push naming a secret its namespace does not declare is refused with 422 before any of its tree is stored, naming the secret and the steps that mount it, once the pusher holds `secret:use`.
- A push whose `metadata.name` is not the workflow it is pushed to, or whose `metadata.namespace` is not its namespace, is refused with 422 at the line (`metadata-name-not-repository`, `metadata-namespace-not-repository`), and a version records only the files, manifests and digests it was judged over.
- A redemption answers a step's `files`: the files its paths and globs select, and each file a long form places elsewhere as an entry of its own with that `to` and the mode asked for (`graph.SelectFiles`), so a runner is handed no URL for a file its step did not select. A relocated file is there instead of under `/agk/repo`, so a step whose `files` only relocate is handed an empty `/agk/repo`.

### Controller

- A task's grant carries its step's `files` (`db.GrantScope.Files`), which its redemption selects the tree by.

### Runner

- Trees are cached by digest inside their namespace, at `<work root>/.trees/<namespace>/objects/`, and each task's tree is hard-linked from there, so a second task of a commit fetches nothing and no namespace's task is laid out from another's files; past 4 GiB, the least recently used go.

### Driver

- The long form of `files` relocates a file, a directory or a glob, each file keeping its path below the glob's segments before its first wildcard, and gives every file it places its `mode`, as copies in the task's working directory removed with it. A file whose mode gives its owner what it gives nobody else is given to the account the container runs as, and refused where that account is a name; a mode with no `to` places at most 256 files, each on a bind of its own. `agk run --local` places the same files from the working tree, which it binds whole, and follows no link out of it.

### agk

- `agk validate`, `agk run --local` and `agk push` refuse a port or a workflow output past 250 characters in the file and in the manifests they read (`graph.Load`, `brick.ParseManifest`, `driver.Docker.Manifest`).
- `agk run --local` says before its first step that it executes the working tree, uncommitted changes included, and is labelled local.
- `agk push` pushes its repository from the root and refuses an entry point below it, naming `git subtree split --prefix <directory>` (`entry-point-below-root`), and a `--namespace` other than the file's `metadata.namespace`.

### Tests

- The corpus's three documents refusing a port or a workflow output of 251 characters are refused, and are read by the stored readings.
- The schemas' repository corpus runs through `version.Check`, each valid case resolving to exactly its `expected.json`, which the vendored wire schema accepts, and each invalid one meeting exactly its `refusal.json`; the fragment corpus runs through `graph.ParseFragment` (schemas `75d34d9` vendored, still `0.3.0`).
- The runner's boundary test refuses package `repo` by name: a runner never speaks git.
- An installation of v0.3.0 is upgraded through `init` and `serve`: its versions, runs and counted objects are unchanged, its runs read and start as before, and its workflows are empty repositories.
- What a runner holds at rest may include a namespace's cache of trees, each file the bytes its name is the digest of, and nothing else of the workflow's.

### Repository

- Package `repo` reads and writes git on the standard library alone, for the repositories the API is to serve: SHA-1 object IDs; commits, trees and tags held to git's own checks of each object, a refusal naming the check as git does (`hasDotgit`, `treeNotSorted`); packs of version 2, their deltas resolved against the pack or, in a thin pack, the repository, and written back whole so that an entry is copied into a fetch as it is stored; indexes of version 2, the same bytes `git index-pack` writes; pkt-line with side-band; and a tree as an `fs.FS`, read as it is walked. A push is bounded in what it sends, holds and unpacks, each bound a constant with its reason. Its readers are fuzzed from what git writes.
- A repository's refs are rows of `workflow_refs`, a branch or a tag each, the default branch held unborn until its first push: a push moves them by compare and swap under a lock on its workflow, all or none, and never deletes the default branch (`db.NS.UpdateRefs`). Its packs are rows of `git_packs`, recorded receiving before their bytes are written and made live in the push's transaction. Both are behind the namespace's row level security.
- Package `repo/store` keeps a repository's packs and their indexes in the object store under `<namespace>/git/<repository>/pack-<checksum>.pack` and `.idx`, the repository a random key that a rename leaves alone, and reads any object of its live packs through their indexes with ranged reads (`artifact.Ranged`, which the built-in store is), each index read once for the process. The orphan sweep never walks them, and a write into a repository's directory as its last pack is removed makes the directory again.

## v0.3.0, 2026-09-28

More than one person: users sign in with a passkey, or a password where the policy allows one; users, groups and service accounts hold roles and denies at namespace and workflow scope, and API tokens act for them, narrowed where a scope says; namespaces carry an owner and quotas; and a namespace a caller holds nothing in is answered as one that does not exist. A v0.2.5 installation upgrades with v0.3.0's `compose.yaml` and its own `.env`, and nothing else.

### Upgrading

- A v0.2.5 installation upgrades with v0.3.0's `compose.yaml` and its own `.env`, and nothing else: `postgres-upgrade` moves its PostgreSQL 17 cluster to 18 with `pg_upgrade` before `postgres` starts, keeping the old one beside it as `postgres-17`, and `init` applies every migration.
- The v0.2.5 operator token is the bootstrap token and goes on working after the upgrade, with nothing to do by hand: `init` keeps its hash in the database from `AGK_OPERATOR_TOKEN`, the API no longer reads `AGK_OPERATOR_TOKEN_FILE`, which a v0.2.5 `compose.yaml` may go on setting, and `operator-token.sha256` is left where it was.
- An administrator the bootstrap token creates is given the `owner` role on every namespace no record names an owner of, recorded as `grant.create` by `operator` in each, so that the first administrator of an upgraded or new installation owns its namespaces with nothing shared by hand; a repeat for a fresh link gives nothing twice.
- The operator token of a server that runs no `init`, Homebrew's or one put together by hand, goes on working too: `agentiik-api migrate` takes `AGK_OPERATOR_TOKEN` as `init` does, and with none set imports, once, the v0.2 hash in the file `AGK_OPERATOR_TOKEN_FILE` names where the database keeps no hash and the bootstrap has not ended; a file that is not there imports nothing, one in another shape or readable by others fails the run.
- A namespace v0.2 made is given its built-in identity, `NS/agentiik`, holding no grant, by the next `init` or `agentiik-api migrate`, with nothing to do by hand; each run gives it to any namespace still without one, recorded as `service_account.create` by `installation` in the namespace.
- A namespace v0.2 made under `stats`, reserved from v0.3.0, keeps its name and is served as before with nothing to do, `AGENTIIK_NAMESPACE` naming it included; `init` and `agentiik-api migrate` say so in one line. Where `AGENTIIK_NAMESPACE` names `stats` and no namespace carries it, `init` creates none, asks for another name and goes on.
- Behind nginx, add `proxy_set_header X-Forwarded-For $remote_addr;` to the `location /` of its configuration: the API now takes a sign-in's address from the last entry of that header behind `AGK_PROXY_URL`, which Caddy and Traefik write already and nginx passes on as the client wrote it.
- A run v0.2 finished, which carries no expiry, is given its namespace's `max_retention_days` from its end by migration 0038, at the next `docker compose up` with nothing to do by hand, so that its envelopes and logs are purged once that has run out.
- A run v0.2.5 left queued, attributed to `operator`, is let in after the upgrade while the bootstrap token lasts; one still waiting when the first administrator signs in ends `cancelled`, naming the end of the bootstrap, and a run already let in is never asked again.
- The artifact files v0.2 recorded no reference for, those of every output given no `retain`, are recorded by the next `init`, or `agentiik-api migrate` where `AGK_OBJECTS_DIR` is set, from the envelopes of the runs it finished, each expiring its namespace's `max_retention_days` after its run finished, with nothing to do by hand; what either leaves, the controller records.

### Images

- `ghcr.io/agentiik/postgres-upgrade` (`build/postgres-upgrade.Dockerfile`), published with the others, carries PostgreSQL 17 and 18 with their contrib modules, and upgrades a data directory an older major version wrote to the one it is given with `pg_upgrade` in copy mode, keeping the old one beside it as `postgres-17`. A new or an upgraded directory it leaves alone, a server killed at its stop it recovers first, one still running it refuses, and a failure changes nothing.

### Access

- `stats` joins the words no namespace, login, group or service account is created under, for v0.6.0's `GET /api/v1/stats/pools` (`agk.LateReservations`); a grant, a token, a workflow, a service account or a route naming an existing one reads it as before.
- A route about its caller's own credentials takes `api.Own` and is registered with `api.Router.HandleOwn`: any principal reaches it, and its handler is told who asks, the token presented, whether it is narrowed, and the namespaces it owns, which the authorizer says as `api.Owners`: those where it holds the `owner` role on the namespace, its own or a group's (`access.Owns`).
- Package `access` resolves permissions with no database, bus or HTTP behind it, so the API and the controller share one rule. `api.Permission` and the nine are its own, under the same names.
- The four roles are fixed permission sets: `viewer` reads, `operator` runs and follows runs without `workflow:read`, `editor` adds writing, run data and secrets, and `owner` adds `workflow:delete` and `grant:manage`. Roles held together add up.
- A principal holds the union of its own and its groups' grants on a namespace and on a workflow. A workflow's grant only adds, and never gives `secret:use` or `secret:write`.
- A deny names one permission and wins over any allow at any scope, and a grant or a deny lapses at its `expires_at`.
- A push of a version naming a secret is refused with 403 unless the pusher holds `secret:use` in the namespace, and a deny of it on the workflow counts; running the version takes `workflow:run` alone. A route declares such a second permission as `api.Needs.Also`, and its handler asks it with `api.HoldsAlso`.
- Package `internal/webauthn` verifies passkey registrations and assertions (Web Authentication Level 3) with the standard library alone: a CBOR decoder of its own, fuzzed, COSE keys ES256, EdDSA and RS256, and the attestation format `none` alone. A signature counter that does not move forward, where it is not zero on both sides, is `ErrPossibleClone`, for the caller to decide on.
- Every request is authorised from the database by `api.Principals`: a bearer token is found by its SHA-256 among the live API tokens, its use recorded, and a route is allowed what the grants of the principal and its groups give, intersected by the router with the token's scope (`access.TokenScope`, carried in `api.Identity`). The installation is an administrator's `grant:manage`, through a token with no scope alone, with no implicit `run:read_data` anywhere.
- The bootstrap token is an administrator owning every namespace, writing as `operator` as the v0.2 operator did, until the first administrator has signed in; from then on it is a 401 that says so. The interim operator is gone.
- `api.OpenSession` opens a browser's session, `__Host-agentiik_session`, 256 bits kept as its SHA-256, HttpOnly, Secure and SameSite=Lax, which `api.Principals` reads beside the bearer token: it ends 12 hours idle and 30 days after it opened, and a revocation, a removed credential or a suspension ends it from the next request.
- A session a password opened enrols passkeys and sets that password, and nothing else, wherever the policy that applies requires a passkey, whatever passkeys its account holds, and opens nothing once passwords are forbidden, read at every request: a policy changed applies from the next. On an installation addressed by an IP address passwords are allowed and no passkey is required, whatever the stored policy says.
- The policy that applies to an account is read in one place by every ceremony, sign-in, session and the sign-in page: the installation's, tightened setting by setting by each namespace the account holds a role in.
- A session a synced passkey opened opens nothing from the next request where `device_bound_only` applies.
- A request changing something that a session carries is a 403 unless its `Origin` is the public URL's, a session that may only enrol is a 403 on every route the router authorises, and a bearer token beside a session is a 400.
- A namespace's record is read by an administrator and by whoever holds a role in it, its own or a group's, through the router's `api.OnNamespace` guard: the authorizer says where a principal holds a grant as `api.Holdings`, and a token's `within` narrows it (`access.TokenScope.Reaches`).
- A route declaring `api.Needs.OrAdministrator` is reached by an administrator as well, whatever they hold at its scope, through a credential that carries the power, and its handler asks `api.Administering` whether the caller came in so; one declaring `api.Needs.Seeing` hands its handler `api.Sees`.
- `api.Caller.Effective` answers what the caller holds at each scope through the credential it presented, resolved from what the authorizer says as `api.Standings`.
- `access.Grant.Gives` and `access.Grant.Takes` answer what one grant gives or denies at a scope whatever its expiry, held to `access.Resolve`, and `access.BootstrapOperator` names the bootstrap token's principal for the API and the controller alike.
- A route naming a run refuses one that is not there, one not under its path's namespace, and a URI that does not parse, after the question it asks of a run that is there, about a stand-in; the router asks the authorizer whether or not the token keeps the permission, and narrows the answer after.
- `GET /api/v1/runs` and `GET /api/v1/{ns}/runs` ask about every workflow they could list as one question, `api.HoldsEach`, which `api.Principals` answers as `api.Among` from the principal and its grants read once.

### API

- `POST /api/v1/auth/tokens` mints an API token, `agktoken_` and 256 bits shown once and kept as its SHA-256, for the caller or a service account of a namespace it owns, expiring in 90 days unless asked otherwise and within a year, a minute more taken as asked for a client's clock running ahead, narrowed by an optional scope; a narrowed token, the bootstrap token and a service account's token asking for that service account mint none (403), none is minted for a namespace's built-in `NS/agentiik` (422), and a principal holding 100 live tokens is refused another (409). Audited as `api_token.create`.
- `GET /api/v1/auth/tokens` lists the tokens still accepted, the caller's and its service accounts', with last use and label, a narrowed token itself alone, and `DELETE /api/v1/auth/tokens/{id}` revokes one from its next request, audited as `api_token.revoke`. A service account's token is audited in its namespace, a user's on the installation.
- `init` keeps the bootstrap token's hash in the database at every run, a changed token replacing it, says at every run that the token set is ignored once the bootstrap has ended, and mints none: with none set and none kept it says that nobody can create the first administrator.
- `init` and `agentiik-api namespace` record their acts, a namespace created and the runner's join token, as `installation` rather than `operator`, which names the bootstrap token from now on.
- A run past `max_runs_per_hour` is answered 429 with `Retry-After`, the seconds until one more fits.
- A redemption by a runner of a pool the task's namespace leaves out of its `allowed_runner_pools` is answered 422, as one by a pool that does not accept the namespace is.
- `/api/v1/namespaces`: an administrator creates a shared namespace with an owner, given the owner role on it in the same act, and quotas naming pools that exist, sets its quotas whole with `PUT .../quotas`, and removes one holding nothing, audited as `namespace.create` beside the owner's `grant.create`, `namespace.update` and `namespace.delete`; whoever holds a grant in one reads it and its quotas.
- A namespace is created with its built-in identity, `NS/agentiik`, holding no grant, and removed with it, its grants and its authentication policy. `agentiik-api namespace` writes through the same store, refuses a user's personal namespace, and counts stored objects and service accounts in what a namespace holds.
- `/api/v1/users` and `/api/v1/groups`, an administrator's, and the bootstrap token's until the first administrator signs in: users and groups created, listed, read and removed, and members put in and out touching no grant, each act audited.
- A user is created with no credential and answered an enrolment link, `…/auth/enrol#agkenrol_…`, single use and good for an hour; asked again before they enrol, or at `POST /api/v1/users/{login}/enrolment`, a fresh one revokes it, a display name or admin left out keeping what was recorded, and a display name left out at creation being the login. The bootstrap token creating an administrator is answered a first administrator's link.
- A login keeps to the namespace grammar, `operator` and `installation` refused, and is a 409 where a namespace holds it. Removing a user takes their empty personal namespace with them, and is refused naming one that holds something or a namespace they own, and, once the bootstrap token has ended, for the last administrator who can sign in.
- `GET` and `POST /api/v1/service-accounts` list the service accounts of the namespaces the caller owns, the built-in `NS/agentiik` of each among them, and create one, `NS/NAME`, in one of them, `agentiik` refused; `DELETE /api/v1/service-accounts/{ns}/{name}` removes one with its tokens and grants, the built-in refused with 409. Audited as `service_account.create` and `service_account.delete` in the namespace.
- `init` says so and goes on where a user's login holds the name of the namespace its settings name, since failing would keep every service from starting.
- `/api/v1/{ns}/grants` and `/api/v1/{ns}/workflows/{name}/grants` list, write and revoke grants and denies behind `grant:manage` at that scope, and an administrator writes one in any namespace; audited as `grant.create` and `grant.delete`. A workflow's list shows its namespace's grants too, each with its scope; a grant is revoked at the scope it was written at; a principal that does not exist, or a service account of a namespace its writer does not see, is 422.
- A grant an administrator writes by the installation's power, and an administrator widening their own access, a role given to themselves, a group they are in or a service account of a namespace they own, or a deny taken from one, tell each of the namespace's owners, `admin_access_widened`, or every holder of its owner role where its record names none, and every other administrator as well where nobody held it before the act.
- An administrator putting anybody in a group tells the owners of each namespace where its grants give a role, and taking themselves out of one or removing one they are in, where its denies took a permission away, `admin_access_widened` with the group's grant, and `group_member.add`, `group_member.remove` or `group.delete` names who was told, by namespace.
- `admin_access_widened` says which act widened access, `act` (`granted`, `deny_lifted`, `joined_group`, `left_group` or `group_removed`), who did it, `by`, and on `joined_group` the user put in, `login`.
- `GET /api/v1/me` answers the caller's record, groups, permissions per namespace and per workflow where they differ, narrowed by its token, and its notifications, kept 90 days; `DELETE /api/v1/me/notifications/{id}` dismisses one. A narrowed token reads and dismisses none.
- `GET /api/v1/me` leaves out a workflow whose denies take everything the caller holds there, since it is one the caller cannot read.
- `POST /api/v1/auth/passkey/options` and `POST /api/v1/auth/passkey/verify` run the passkey ceremonies, the public URL's host being the Relying Party and its origin the only one accepted: a challenge single use and good for 5 minutes, discoverable credentials, the user verification the policy requires, attestation `none`. An installation addressed by an IP address answers 409, and a request from another origin 403.
- A registration from an enrolment link's code or a recovery code records the passkey, spends the code and signs its user in, a user suspended for anything but holding no passkey being signed in by neither; one from a session adds a passkey to its user. Either ends the bootstrap token where it is an administrator's, not suspended, and the token has not ended. Audited as `credential.enrol`, `enrolment.use`, `bootstrap.end` and `signin.succeed`.
- An assertion opens a full session and records the passkey's counter, Backup State and last use. A counter that did not move forward is refused, stores nothing and writes the user a `passkey_counter_refused` notification, and a synced passkey where `device_bound_only` applies is a 403 naming it; each refusal is audited as `signin.fail`, those before a signature verified at most ten per address and a hundred in all in ten minutes, the next entry counting those left out.
- An enrolment link or a recovery code refused, presented to `POST /api/v1/auth/passkey/options` or `POST /api/v1/auth/password/enrol`, a password it would set where passwords are forbidden included, and a registration it started refused, or one answering no challenge, are audited as `signin.fail` within the same bound, naming the code's account, or `an unknown enrolment code`, and why it was refused, never the code.
- A user's first sign-in creates their personal namespace, owned by them with the owner role and its `NS/agentiik`, recorded as the installation's act.
- `POST /api/v1/auth/login` signs in with a password, checked against its Argon2id hash at the OWASP baseline (19 MiB, 2 iterations, 1 lane, `internal/password`), and a TOTP code where the account holds a generator (RFC 6238: HMAC-SHA-1, 30 s, 6 digits, a step either side, no code accepted twice, `internal/totp`), from the sign-in page's origin alone. Passwords forbidden by the policy that applies to the account are a 403 naming `password` before any is compared; every other refusal is one 401. Audited as `signin.succeed` and `signin.fail`, within the passkeys' bound.
- `secret.TOTP` seals a TOTP generator's secret under the master key, bound to its user and its identifier, and opens it for the password sign-in (`api.TOTPSecrets`).
- Password attempts are counted, ten for one login and thirty from one address in a sliding quarter of an hour, and one past either is a 429 with `Retry-After`, before anything is read or hashed; a sign-in starts its login's count again, and a refusal by the policy counts against its address alone.
- At most one password hash per processor runs at once, within a quarter of `GOMEMLIMIT` or the container's memory, and a sign-in waiting 5 s for its turn is a 503.
- Behind `AGK_PROXY_URL` a sign-in's address, which `signin.fail` names and the bounds count, is the last entry of `X-Forwarded-For`, and nowhere else; the passkey ceremonies and the password sign-in share that bound (`api.SignIns`).
- `GET /auth/sign-in` and `GET /auth/enrol` serve the API's own sign-in and enrolment page on the public URL's origin, embedded in `agentiik-api`: static HTML, a stylesheet and three scripts, under a Content-Security-Policy that loads nothing from elsewhere and runs no inline script, `Referrer-Policy: no-referrer` and nosniff, the HTML never cached. It runs both passkey ceremonies with fetch, says who the browser is signed in as and offers the sign-out, to a session that may only enrol too, reads an enrolment link's code after its `#` or takes a recovery code typed in, says passkeys are unavailable where the installation is addressed by an IP address, carries `agk login`'s `redirect_uri` and `code_challenge` on their grammar alone (a 400 page otherwise), and offers a password form only where `POST /api/v1/auth/login` is served and the policy lets passwords in.
- The sign-in page shows a session that may only enrol the way to enrol a passkey and its sign-out and no sign-in, and says in minutes how long `Retry-After` asks to wait after too many attempts.
- `POST /api/v1/auth/sign-out` revokes the session its request carries, one that may only enrol included, and clears its cookie, 204; from another origin it is a 403, and carrying a bearer token or two credentials a 400.
- A write to the built-in store, a policy's form or a presigned PUT, that would take its namespace past `max_artifact_bytes` is answered 507 with nothing stored, an envelope's as an artifact's, which a runner reads as `artifact.ErrNoRoom` and fails the step on the platform's account. What counts is the namespace's live artifacts, each digest once, and its uploads not yet referenced, room being made at the request's length, or the room left up to `artifact_max_bytes` where it states none, under a lock on the namespace's room before the bytes are read, so two writes at once cannot both take the last of it. An object the namespace holds as a live artifact takes none, and a namespace with no such quota, as every upgraded one, is refused nothing and locks nothing.
- Every write through the built-in store's routes, a policy's form or a presigned PUT, in a namespace with `max_artifact_bytes` or without, is recorded as under way before its bytes are read, until the collection's 24-hour grace past its policy, so that the collection leaves its object alone until the result that references it is heard; a write whose bytes are refused lets go of it.
- A push writes a tree file again where its version had to record the object afresh and the store no longer holds its bytes, as a collection that died before confirming and the next one leave it.
- `POST /api/v1/auth/password/enrol` sets a password from an enrolment link's code or a recovery code where the policy allows passwords, as it always does on an installation addressed by an IP address, spending the code and opening the session a password opens. An administrator's ends the bootstrap token where that session is a full one, and otherwise their first passkey, registered from it, does. A recovery code replaces the password held, removes the TOTP generator beside it and ends the sessions it opened. Audited as `credential.enrol`, `credential.remove`, `enrolment.use`, `bootstrap.end` and `signin.succeed`.
- An administrator's password sign-in to a full session ends the bootstrap token where it has not ended, as their enrolment does, so that a policy relaxed after their password was set ends the token at their next sign-in. Audited as `bootstrap.end`.
- The first request of an administrator's session that has come to be full, one a password opened to enrol only before the policy relaxed, ends the bootstrap token where it has not ended. Audited as `bootstrap.end`.
- The API's 401 to the bootstrap token once it has ended, `init` and the `reason` of an `operator` run cancelled for it say it ended when the first administrator signed in.
- `PUT /api/v1/me/password` sets or changes the caller's password from a browser's session, one that may only enrol included, the current one required where one is held and counted as a guess, ending every other session the old one opened; `DELETE /api/v1/me/password` removes it with the generator beside it, refused with 409 naming `min_passkeys` below it, and always on an installation addressed by an IP address. A password is 12 characters to 1024 bytes and not the login, with no composition rule. Audited as `credential.enrol` and `credential.remove`.
- `POST /api/v1/me/totp` starts a TOTP generator beside the password and answers its secret and `otpauth://` URI once; it counts for nothing until `POST /api/v1/me/totp/confirm` sends a code it shows within ten minutes, and `DELETE /api/v1/me/totp` removes it with a code it shows, counted as a guess. Audited as `credential.enrol` and `credential.remove`.
- Setting a password or a generator where the policy that applies to the account forbids passwords is a 403 naming `password`, and a bearer token sets and removes neither (403).
- Setting a password, from a session or a code, where the policy requires a passkey and the account holds the `min_passkeys` passkeys it accepts is a 409 naming `passkey`: nothing is set and the code is not spent.
- The enrolment page sets a password from its link or a recovery code, beside the passkey or in its place on an installation addressed by an IP address, and both pages offer a signed-in browser its password and a generator, whose key they show as text and as a QR code drawn in SVG by an encoder of their own, `qr.js`.
- `GET /api/v1/runs/{id}` answers `reason` for a run the controller cancelled at creation, its principal no longer holding `workflow:run`.
- `POST /api/v1/users/{login}/recovery` issues a user a recovery code, whatever they hold, shown once with the link to `/auth/enrol` that carries it, single use and good for an hour, revoking their open one; it enrols a passkey, or a password where the policy allows passwords. Audited as `enrolment.issue` of the kind `recovery`, by the administrator for the user; their own account is a 403, a service account a 404. The bootstrap token's open nothing once it has ended.
- `agentiik-api recover LOGIN`, the break-glass path, taking no credential, issues an administrator a recovery code from the API's database settings and public URL and prints its link once; recorded as `enrolment.issue` by `installation`, and refused for a user who is not an administrator.
- The break-glass path is told to every administrator, the one recovered included: a `break_glass_recovery` notification in `GET /api/v1/me` naming the account as `login`, and when, the `enrolment.issue` entry naming who was told.
- A first administrator's or a new user's link opens nothing once its user holds a credential, however it came, so that one left open beside a recovery code that enrolled them is a 401.
- `GET /api/v1/auth/policy` answers the installation's authentication policy, every setting written, to any authenticated caller but a session that may only enrol, and `PUT` replaces it whole, a setting left out at its default, an administrator's alone. Audited as `policy.change` with the policy as it was.
- `GET /api/v1/{ns}/auth/policy` answers what a namespace tightens to an administrator and to whoever holds a grant in it, and `PUT` replaces it, an administrator's alone, a setting looser than the installation's refused with 409 naming it and `{}` tightening nothing. Audited as `policy.change` in the namespace.
- Forbidding passwords, on the installation or in a namespace, deletes the passwords and TOTP generators it reaches as rows in the same transaction, ending their sessions, and suspends each account it reaches that holds no passkey the policy accepts, recorded as `no_passkey`; it is a 409 naming `password` on an installation addressed by an IP address.
- Each password a policy change takes, and the generator beside it, is audited as `credential.remove` by whoever changed the policy, after its `policy.change`.
- A user's record, in `GET /api/v1/users`, `GET /api/v1/users/{login}` and `GET /api/v1/me`, answers `suspended_for`, `no_passkey`, where the policy suspended the account, and nothing for a suspension it did not make.
- A change of the policy that would leave no administrator able to sign in, once the bootstrap token has ended, is a 409 naming `password` or `device_bound_only`, whichever takes their way in.
- A grant carrying a role, and a user put in a group, that would leave no administrator able to sign in once the bootstrap token has ended, the last of them brought under a namespace's policy that refuses their way in, is a 409 naming `password` or `device_bound_only`, and writes nothing.
- The last administrator who can sign in, not removed, is counted as the policy that applies to them says: a synced passkey where `device_bound_only` applies, or a passkey on an installation addressed by an IP address, signs nobody in.
- A sign-in refused because passwords are forbidden to its account deletes the password it was offered, and the generator beside it, which a grant given since in a namespace forbidding them had left.
- The passkey that brings an account to `min_passkeys` passkeys the policy accepts, where a passkey is required or passwords are forbidden, deletes its password and the generator beside it, and the session the password opened.
- A passkey enrolled from an enrolment link or a recovery code lifts a suspension made for holding no passkey and signs its user in, and the first administrator's then ends the bootstrap token.
- A password set from an enrolment link or a recovery code lifts a suspension made for holding no passkey and signs its user in, recorded as `suspension_lifted` on its `credential.enrol`.
- `GET /api/v1/me/credentials` lists the caller's passkeys, password and generator, with kind, flags, label, creation and last use, never a key, a hash or a secret, and `DELETE /api/v1/me/credentials/{id}` removes one from a browser's session, ending the sessions it opened: 409 naming `min_passkeys` for a passkey the policy accepts below it or the password where fewer are held, 409 for the password on an installation addressed by an IP address, for the last credential, and for a generator, which takes a code at `DELETE /api/v1/me/totp`. Audited as `credential.remove`.
- A passkey registered from a session, a first password and a TOTP generator need a sign-in within the last 10 minutes, and are otherwise a 403 carrying `WWW-Authenticate: Bearer error="insufficient_user_authentication", max_age="600"`; a password changed is proved by the current one sent with it.
- The sign-in and enrolment pages list a signed-in account's credentials with a button removing each, ask for a sign-in again, by passkey or password, where adding a credential asks for a recent one, say when a passkey took the password and its session, and offer the signed-in section's password forms as the account's own policy says.
- A passkey or password sign-in carrying agk login's `terminal` mints a one-time code, `agkcode_` and 256 bits kept as its SHA-256, single use for a minute and bound to the challenge, the account and the credential that signed in, and answers `redirect_to`, the loopback address with the code; a session that may only enrol is handed none, and `terminal` beside a registration or off its grammar is a 400.
- `POST /api/v1/auth/exchange` trades that code and the verifier whose SHA-256 is its challenge for a 90-day API token of whoever signed in, audited as `api_token.create` with the credential that signed in. The code is spent by the first exchange its schema accepts; one used, lapsed, of another verifier, of an account suspended or removed since or of a password set anew since is one 401, one a password minted where the policy now requires a passkey or forbids passwords, or a synced passkey where `device_bound_only` now applies, a 403, and a principal at 100 live tokens a 409.
- An exchange refused once it holds to its schema is recorded as `signin.fail` with its reason, by the address it came from, about the account the code names, or `an unknown exchange code`, within the bound the sign-ins share; one refused after its verifier answered is recorded whatever the bound says.
- A grant naming `installation` is refused with 400, as one naming `operator` is, since neither is a principal the wire can name.
- A deny of `grant:manage` on a workflow is refused with 422, since it would take from whoever it names the permission that revokes it: nobody locks a namespace's owners out of a workflow.
- A machine joining a pool is recorded as `runner.join` on the installation, by whoever issued its join token, `installation` for the one `init` issues, with the runner, its pool and its labels.
- At most 10,000 passkey challenges are open at once across the installation (`db.ChallengesLive`): past it, `POST /api/v1/auth/passkey/options` answers 503 with `Retry-After`, the seconds until the oldest lapses, and writes nothing.

### State

- Migration 0047 reserves `stats` in `agentiik_reserved`.
- A session is opened by a credential alone, never by an enrolment code: migration 0048 drops `sessions.enrolment_code`, which migration 0032 added.
- `db.Wide.TokensOf` lists only the tokens still accepted, of a principal and of the service accounts of the namespaces named, and `db.Wide.Token` reads one by its identifier.
- Migration 0032 adds the identity and access tables and a namespace's kind, owner and four new quotas; `init` upgrades a v0.2.5 database at the next `docker compose up` with its rows as they were.
- Package `db` reads and writes them, finding tokens, sessions and enrolment codes by the SHA-256 of their value and only while they are live, and grants as package `access` resolves them, under two new reasons, `Identity` and `Authorisation`.
- A suspended user's enrolment link and recovery code still work, since enrolling is how such an account comes back; a credential of theirs opens no session.
- `db.NS.CreateRun` refuses a run past the namespace's `max_runs_per_hour`, a sliding count of the last 60 minutes whatever started the runs, with `db.RunsPerHourReached`, counting under a lock on the namespace so that replicas of the API count one after the other; migration 0033 indexes runs for it. A namespace with no such quota, as every upgraded one, is refused nothing and locks nothing.
- `db.Wide.CreateNamespace` takes a namespace's kind, owner and quotas, `db.Wide.GrantAccess` writes a grant in any namespace and `db.Wide.AuditIn` records an act done in one, for the owner's grant at creation.
- `db.NS.CreateRun` attributes a run of a schedule, a webhook or an event (`agk.TriggerKind.Unattended`) to its namespace's built-in identity, `NS/agentiik`, and refuses one naming anybody else, so that no later trigger attributes its runs to the workflow's last editor.
- `db.Wide` lists, reads and removes service accounts, refusing the built-in identity with `db.ErrBuiltIn`, gives namespaces their missing built-in identities, and counts a principal's live tokens under a lock on it (`LiveTokens`).
- Migration 0034 adds `notifications`, one row per reader: `db.NS.TellOwners` writes them in the grant's transaction, `db.Wide.NotificationsOf` reads a reader's and removes those past 90 days, `db.Wide.DismissNotification` removes one.
- `db.Wide.ShutEnrolmentCode` answers an enrolment code whatever its state, and why it opens nothing, and `db.Wide.TellOwnersIn` tells a namespace's owners from a transaction across the installation.
- `db.NS.RevokeAccess` holds the namespace before its grant, and `db.NS.TellOwners` the namespace and then each owner it tells, in the order their removal takes them, so that an act telling the owners beside the removal of the namespace, or of an owner, waits for it and tells nobody gone, rather than failing on a deadlock or a reference.
- `db.NS.AccessGrantsAt` lists what applies at a scope, `db.NS.RevokeAccess` revokes a grant at the scope it was written at and answers it, and `db.NS.Present` tells a namespace or a workflow that is not there.
- Migration 0035 adds `users.webauthn_handle`, 32 random bytes minted at a user's first registration, and `webauthn_challenges`.
- `db.Wide.EndSession` revokes a session by its hash whether it opens anything now or not, so that one signed out of while its user is suspended stays ended when the suspension is lifted.
- Migration 0036 adds `artifact_uploads`, one row per write not yet referenced, lapsing a quarter of an hour after its policy, and `artifact_room`, what a namespace with `max_artifact_bytes` holds as last counted, counted whole again once a minute old or before a refusal on a count over a second old, and indexes the live artifacts by digest: `db.NS.MakeRoom` holds room for a `db.Upload` or refuses it with `db.NoRoom`, and `db.NS.Stored` and `db.NS.Unwritten` settle it.
- An artifact of a workflow that declares no `retain` is recorded, kept as long as the namespace's `max_retention_days`, rather than left for nothing to expire, collect or count.
- A finished run's envelopes and logs expire once its `defaults.retain` has run, capped at the namespace's `max_retention_days`, which also bounds a workflow declaring none: `db.Decision.Retain` replaces `ExpiresAt`, which nothing set.
- `db.Wide.Consumption` reads what each namespace holds against its quotas, and `db.Evaluation.MaxRunDuration` its bound with the run.
- Migration 0037 adds `credentials.totp_step`, the step a TOTP code was last accepted at, which `db.Wide.TOTPUsed` moves forward and never back (`db.ErrTOTPSpent`), and holds a TOTP beside a password: refused to a user holding none (`db.ErrNoPassword`) and removed with it however it goes.
- `db.Session.CredentialType` says what opened a session.
- Migration 0038 adds `runs.envelopes_purged_at` and `runs.logs_purged_at`, and indexes the runs each purge has left, so that a purge reads its work and never every run it purged.
- `db.Pool.PurgeEnvelopes` takes a run's row and stamps it before lowering its counts, a batch in one statement, so two purges at once never lower them twice.
- `db.Pool.ExpireArtifacts` and `db.Pool.Collected` take only rows nobody holds, and never deadlock with a decision.
- `db.Pool.LogsGone` stamps a run none of whose tasks holds a log, once no shipment can be under way, and `db.Pool.PurgeUploads` forgets the writes that have lapsed.
- `db.Pool.Collecting` deletes what `Collectable` claimed through the caller, under a lock on each row; neither takes an object a live artifact names or a write holds.
- `db.NS.Uploading` records a write before its bytes are read, holding the object's row, and `db.NS.NotWritten` lets go of one refused.
- Migration 0039 adds `totp_enrolments`, the TOTP generators waiting for their first code. `db.Wide.SetPassword` sets a password in place of the one held, keeping its identifier, `db.Wide.EndSessionsOpenedBy` ends the sessions a credential opened, and `db.Wide.AddCredential` takes the step of the code confirming a generator.
- Migration 0040 adds `runs.reason`, written by `db.Decision.Reason` and read as `db.RunDetail.Reason`, and indexes the audit log's `grant.delete` entries; `db.Wide.Attribution` reads a run's principal as it stands in a namespace, and `db.Wide.Revocations` the grants revoked from it, as the audit log recorded them.
- Migration 0041 adds `runs.files_recorded`, which every decision sets, so that a finished run without it is one whose files a v0.2 controller left unrecorded: `db.Pool.UnrecordedRuns` reads those with the envelopes their steps published, and `db.Pool.RecordUnrecorded` records the files and sets the column in one transaction, giving a run with no expiry its namespace's `max_retention_days`, and leaves a run whose object a writer holds rather than wait for it.
- `db.Pool.Sweepable`, `Unnamed`, `LiveEnvelopes` and `Orphaned` find the files of the store no row names, in a namespace with no finished run still to be recorded, and hand them to the collection as rows counting nothing, collectable from then.
- `db.Wide.EnrolmentCodeByHash` and `db.Wide.UseEnrolmentCode` answer no recovery code the bootstrap token issued once it has ended, and no first administrator's or new user's link whose user holds a credential.
- Migration 0042 adds `users.suspended_for`, why the policy suspended an account: `db.Wide.Suspend` writes it, `db.Wide.LiftSuspension` lifts a suspension made for that reason alone, and `db.Wide.UsersUnderPolicy` answers who holds a role in a namespace. `db.Session.BackupEligible` says whether a synced passkey opened a session. `db.Wide.HoldUser` and `db.Wide.Administrators` hold a user's row as a reference to it does not wait on, so that a membership written with two users never deadlocks with an act holding every user.
- Migration 0043 adds `notifications.login` and the kind `break_glass_recovery`, which `db.Wide.TellAdministrators` writes to every administrator.
- Migration 0044 adds `exchange_codes`, agk login's one-time codes: `db.Wide.IssueExchangeCode` keeps one, removing some past their minute, `db.Wide.TakeExchangeCode` takes one once, and `db.Wide.SetPassword` removes those the password it replaces minted.
- `db.Wide.Present` and `db.Wide.TellOwners` do what `db.NS`'s do, for a grant written through the installation's handle, which reads who can sign in across the namespaces in the same transaction; `db.Session.Admin` says whether a session's user administers the installation.
- Migration 0045 puts `audit_log`, `auth_policy`, `notifications` and `service_accounts` behind the namespace's row level security, as every other table naming a namespace is: each is read through the installation's door, and a handle on one namespace reads its own rows of them and nothing else.
- Migration 0046 adds `notifications.act` and `notifications.acted_by`, which `db.Widening` fills, keeps the notifications written before it where they name who acted and removes the rest; `db.NS.TellOwners`, `db.Wide.TellOwners` and `db.Wide.TellOwnersIn` take a `db.Widening`.
- `db.Wide.HoldMember` holds a user's personal namespace and principal, as their removal takes them, and a membership takes it before its insert, so that the two take turns rather than deadlock.

### Artifacts

- `artifact.Dir` answers an `artifact.Removable`, whose `Remove` deletes an object and the log directories it leaves empty, and says whether there was one to delete.
- `artifact.Walkable`, which the store `artifact.Dir` opens is, lists the objects of one namespace a batch at a time, with their size and when they were written, and nothing else in their directory.

### Controller

- A step's pool is chosen among those its namespace's `allowed_runner_pools` names, where it names any: a step whose labels only a pool outside them carries fails with 125 naming the list.
- A run's root `timeout` is held to its namespace's `max_run_duration`, read at every pass, and a workflow writing none is bounded by it; `graph.New` and `graph.Options.MaxRunDuration` take the bound, zero bounding nothing.
- `agentiik_quota_used` and `agentiik_quota_limit`, by namespace and quota, for `max_concurrent_tasks`, `max_runs_per_hour` and `max_artifact_bytes`, keeping 1,000 namespaces and summing the rest under `namespace="_other"`, each quota apart (`metrics.Desc.FoldBy`).
- The controller that leads runs the retention purges and the collection, package `purge`, as its term begins and every ten minutes, a batch a call and at most ten calls of each purge a pass, the rest left to the next: references past their `retain` are retired, a finished run's envelopes and logs go once its retention has run out, and an object leaves `AGK_OBJECTS_DIR` once nothing has counted it for the 24-hour grace, no live artifact names it and no write of it is under way. A pass that removed something says what in one line, and one that failed says why.
- `agentiik_artifacts_expired_total`, `agentiik_runs_purged_total`, `agentiik_logs_purged_total`, `agentiik_objects_collected_total` and `agentiik_objects_collected_bytes_total` count what the purges removed.
- A run is let in only while its principal holds `workflow:run` on its workflow, asked of package `access` on every pass until it is admitted and before its concurrency group; one that no longer does ends `cancelled` before any task. Its reason names the grant that lapsed by its identifier, role and scope, says that a deny applies or that the principal holds nothing there, or names the end of the bootstrap token; the whole account, the group, the deny, the suspension or removal and who revoked, goes to its `run.cancel` entry by `installation` alone.
- A decision records the files of every envelope its run keeps, a shard's included where its step never published, and a file living by the workflow's defaults is kept as long as its run keeps the envelopes naming it, from the run's end rather than from when it was written; an output declaring its own `retain` keeps the instant it declared.
- A purge pass also records what `init` and `migrate` left of those files, and hands the collection each file of `AGK_OBJECTS_DIR` that no row, no write and no envelope of a run under way names once it is a day old, for deletion a day later; `agentiik_orphans_found_total` counts them.

### Driver

- Attaching to a container no longer races its context watcher, which could read the stop channel before it was set.

### Tests

- The vendored schemas are agentiik/schemas v0.3.0's, and tests hold the permission and role enumerations and the principal grammars to the Go vocabulary. The corpus's three documents refusing a port or a workflow output of 251 to 255 characters are skipped, naming v0.4.0, which the documentation gives that refusal to.
- A fan-out of ten thousand items is handed its namespace's `max_concurrent_tasks` and no more, and another namespace's run is handed its task on the same sweep.
- A test holds that no keyword of `workflow.schema.json`, and no field of a parsed workflow or of its graph, confers access.
- A test holds every route `serve` registers to the permission and scope the documentation's API table names, and another upgrades a database v0.2.5 left through `init` and `serve` and uses the same operator token on it; `db.MigrateThrough` migrates as far as a release did, for such tests.
- A test holds every route `serve` registers that changes something to the audit entries its act appends, action, actor, target and namespace, or to a reason it records none, and lists every route that only reads; another holds a flood of failed sign-ins to the bound, beside a grant appending at the same moment.
- Package `internal/webauthn/webauthntest` is a software authenticator answering the API's options as a browser would, held to `internal/webauthn` by its tests.
- A test holds every record a person signs in with or through, credentials, sessions and enrolment codes, refused to a service account; a sign-in path added later joins it.
- The tests, CI and `e2e` run PostgreSQL 18, and a job upgrades with `postgres-upgrade` a cluster the official 17 image wrote, starts 18 on it, and checks what it refuses and what it recovers from.
- A test holds that no file the module ships, the migrations and what `//go:embed` carries included, imports a mail package, requires a module for mail, or holds an SMTP transport, `sendmail`, a `mailto:` link, the word email or a mail provider's name.
- The fake Docker daemon answers a start once the goroutine standing in for the container's process is running and about to call its function, as a daemon answers once the process runs, rather than while that goroutine may not have been scheduled yet, when a container killed and started again at once could have its second run's function called first.
- A test asks every route `serve` registers that names something in its path about what does not exist and about what exists that the caller cannot see, by token, narrowed token and session, and holds the two answers to one status, body and header set; so are the routes naming a namespace's service account in their body or a namespace in their query.
- A test asks every route `serve` registers, as a namespace's owner and as an administrator, and finds a stored secret's value in no answer, as it is, in base64 or in hexadecimal.
- A test holds a run's views to its runner's identifier, and its listings and log stream to no runner at all.
- A test holds the secret routes to namespace grants with real principals: a namespace's viewer reads the declarations and writes none, and an editor of one workflow neither reads nor writes them, nor pushes a version naming one.
- A test follows the presigned URL of an artifact, and holds its signature to the artifact's namespace, digest and run.
- Tests hold the router to one question about a workflow before any refusal of a run, and `api.Principals` to the same transactions whatever the targets it is asked about as one question; `TestAnAbsentNameAndAnInvisibleOneTakeAsLongToRefuse`, run with `AGENTIIK_TEST_TIMING` set, measures both refusals on the routes a prober asks first.
- A test holds every table naming a namespace, asked of the catalog, behind row level security enabled and forced under the namespace's one policy, and every column of namespace names outside it to a decision.
- Package `internal/accesstest` stands the access fixture up through the API's routes alone: finance and hr, alice, bob and an administrator, team-finance, finance/nightly-sync with its token, and grants and denies at both scopes, the page's figure among them and two that lapse.
- A test asks every route `serve` registers, as every principal by token, narrowed token and browser session, about everything of the fixture, at the instant before a lapse and at it, and holds each answer to the page: let through, or refused as its scope refuses, and carrying no secret's value; so are `GET /api/v1/me` and the listings of runs, namespaces, service accounts, tokens and grants, and the route table the guard test reads is the suite's `accesstest.Cases`.
- The v0.3.0 fact, a principal holding no permission in a namespace unable to establish that it exists by any route, error or timing, is a test of its own, in process and on the `e2e` installation; CI times it and the isolation test's refusals with `AGENTIIK_TEST_TIMING` set, without `-race`.

### agk

- `agk token create [--for NS/NAME] [--expires 30d] [--scope ...] [--label TEXT]` prints the token alone on standard output, and `agk token list` and `agk token revoke ID` list and revoke, with `-o json` on create and list. A mint or a revocation answered with a 5xx leaves with 4 and says how to read it back.
- `agk run` says a 429 at the start as a refusal, exit 1, since no run was written, rather than as no outcome.
- `agk login [--server URL] [--label TEXT]` opens the sign-in page in the browser, or prints it with how to forward its loopback port over SSH, takes the one code the browser brings back to `127.0.0.1` within 5 minutes, trades it with its PKCE verifier for a token and keeps it in `agentiik/profile.json` under the user's configuration directory, 0600 in a directory 0700, one per installation, revoking the one it replaces.
- agk presents the token kept for the installation `--server` or `AGENTIIK_SERVER` names wherever `AGENTIIK_TOKEN` is not set, and names it when the installation refuses it; `agk logout` revokes it and forgets it, keeping it where the installation does not answer.
- The profile remembers the installation `agk login` last signed in to, and agk talks to it wherever neither `--server` nor `AGENTIIK_SERVER` names one, as `agk login` says, presenting the token kept for it: `AGENTIIK_TOKEN` set with no address is refused there, exit 2, as is a profile agk cannot read. `agk logout` leaves it remembered.
- `agk namespace create`, `list`, `show`, `delete` and `quotas`. `quotas` reads the quotas held, sets the flags given on top and sends that whole set, lifting a bound only where `--lift NAME` names it. A change answered with a 5xx leaves with 4.
- `agk user create LOGIN [--admin] [--display-name NAME]` sends only what it is given and prints the enrolment link, a fresh one when run again before the user enrols, which enrols a passkey, or a password where the installation allows one; with the bootstrap token and `--admin` it says the token works until LOGIN has signed in. `agk user list`, `show` and `delete`, and `agk group create`, `list`, `show`, `delete`, `add` and `remove`, with `-o json` where they read.
- `agk service-account create NS/NAME`, `list [NS]` and `delete NS/NAME`, with `-o json` on create and list. A change answered with a 5xx leaves with 4, a list with 1.
- `agk auth policy [--namespace NS] [--password allowed|forbidden] [--passkey optional|required] [--user-verification required|preferred] [--device-bound-only[=false]] [--min-passkeys N] [--inherit SETTING]` prints the policy, and with settings given reads it, sets them on top and sends the whole of it; `--inherit` drops a namespace's setting. A change answered with a 5xx leaves with 4.
- `agk share NS[/WORKFLOW] --user L|--group G|--service-account NS/N --role R|--deny P [--expires D]` and `agk share NS[/WORKFLOW] --revoke ID`, `agk grants NS[/WORKFLOW]`, one grant a line with its scope and what it gives there, and `agk whoami [NS[/WORKFLOW]]`, with what the installation tells where it names no scope.
- `agk status` says why a run refused at creation was cancelled, `cancelled: ` and its reason, and `agk run` ends its report with it.
- `agk user recover LOGIN` prints the link of a recovery code for a user, and refuses the caller's own account and a service account in the installation's words.
- `agk whoami` says which act widened access and who did it, whom an administrator put in a group, and when `agentiik-api recover` issued an administrator a recovery code; `agk whoami NS/WORKFLOW` says a workflow with no permissions of its own holds its namespace's unless denies there take all of it, since the installation names no workflow its caller cannot read.

## v0.2.5, 2026-09-26

- Nothing changed here. The version moves because every repository carries the same one, which [Versioning](https://agentiik.github.io/docs#versioning) sets out.

## v0.2.4, 2026-09-26

A server installed from one Compose file: `agentiik-api init` prepares and reconciles the installation at every start, the runner drops from root and joins on its own, step secrets are on a tmpfs volume of their own, and the API renews the control plane's bus credential.

### Upgrading

- The Compose file gains a `bus` volume, mounted at `/init/bus` in `init` and at `/bus` in the API and, read only, the controller, with `AGK_BUS_CREDENTIALS_FILE=/bus/control-plane.creds` in both; the next `init` moves the credential there, and refuses to run under a Compose file without that volume.
- Values a runner before this one wrote under `<secrets_dir>/agentiik` are removed by nothing now; the runner names the path once at start, and the host tmpfs can go once it is empty.

### API

- `agentiik-api init` prepares an installation from `AGK_*` variables alone and brings it back in line with them at every run, for a Compose `init` service: certificate, keys, bus identity and `nats.conf`, migration, namespace, the operator token's hash, and a join token of the pool `default` for the local runner.
- `AGK_PROXY_URL` puts the API behind a proxy on the same host: reached at that URL, plain HTTP on the loopback, `AGK_PUBLIC_URL` and the TLS pair left unread.
- `agentiik-api health` exits 0 once the API beside it answers, for a Compose health check in the image, which has no shell.
- `agentiik-api serve` takes a control plane bus credential renewed in `AGK_BUS_CREDENTIALS_FILE` when the bus drops the old one at its expiry, with no restart.
- `agentiik-api serve` renews the control plane bus credential itself, at start and daily, from 14 days before its expiry, in `AGK_BUS_CREDENTIALS_FILE`, and warns only where it cannot; one that expired while it was down is renewed before its settings are read.
- `init` keeps the control plane bus credential in a new `bus` directory of `AGK_INIT_DIR`, shared by the API (read and write) and the controller (read only), and moves it there from `api/bus` and `controller/bus`.
- `init` names no variable when it mints an operator token, and says to keep it where the installation's settings are.

### Controller

- `agentiik-controller` takes a renewed bus credential likewise, and no longer ends at the old one's expiry when the file holds a later one.

### Images

- `ghcr.io/agentiik/runner` starts as root and `serve` drops to 65532 itself, so its container takes `cap_add` SETUID and SETGID beside CHOWN, FOWNER and DAC_OVERRIDE, and no `user` or `group_add`.

### Runner

- `agk-runner serve` started as root gives the key's directory, the work root and `runner.env`'s directory to `agentiik`, takes the group owning the Docker socket, drops to `agentiik` and starts itself again. It still never serves as root.
- `serve` given `AGK_RUNNER_JOIN_TOKEN` or `AGK_RUNNER_JOIN_TOKEN_FILE` joins when the host has no identity, waiting up to 5 minutes for an API that does not answer, and joins again as a new runner when its key or `runner.env` is gone, or when `AGK_API`, `AGK_RUNNER_LABELS` or `AGK_RUNNER_NAMESPACES` in its environment differ from what it joined with, an unset one claiming none.
- A task's secret values reach its container on a tmpfs volume of its own, filled by the static helper and removed with the task, instead of a bind from a host tmpfs: the host prepares nothing for them.
- `secrets_dir` in `runner.toml` is read and no longer used, and the runner says so once at start.
- `Sweep` removes the secrets volumes and their holders a runner that died left.
- A task given a secret on a runner with no helper is refused before anything is pulled.

### Driver

- `Policy.SecretsDir`, `Policy.RequireSecretsTmpfs`, `SecretsFloor` and `ErrSecretsTmpfsRequired` are gone; `LabelSecrets`, `HolderCommand` and `Policy.SecretsDirSkipped` are new.

### Tests

- `e2e` stands the installation up as the Compose file does: `agentiik-api init` and the three images, the API behind `AGK_PROXY_URL` checked with `agentiik-api health`, and runners that start as root and join on their own; no secrets volume is left on a runner's daemon after a run.

### agk

- `agk run --local` gives secret values the same tmpfs volume, so on macOS they no longer touch the working directory on disk; a build carrying no helper refuses a step given a secret.

## v0.2.3, 2026-09-26

- Nothing changed here. The version moves because every repository carries the same one, which [Versioning](https://agentiik.github.io/docs#versioning) sets out.

## v0.2.2, 2026-09-26

- Nothing changed here. The version moves because every repository carries the same one, which [Versioning](https://agentiik.github.io/docs#versioning) sets out.

## v0.2.1, 2026-09-26

A server a person can install: the images are published, a runner joins the pool `default` with no label, and namespaces are created from the server. The installations themselves are in `agentiik/deploy` and `agentiik/homebrew-tap`.

### Images

- `ghcr.io/agentiik/api`, `controller` and `runner` are published for linux/amd64 and linux/arm64: `X.Y.Z` and `vX.Y.Z` at every release tag, `latest` on the highest release, `dev` at every commit to main. Any other branch builds and checks them and pushes nothing.

### Runner

- A runner may claim no label: `agk-runner join` without `--labels` joins the token's pool claiming none, which with a token of the pool `default` takes the steps naming no `runs_on`. `serve` refuses `AGK_RUNNER_LABELS` in the environment of a runner that joined claiming none.
- `agk-runner join --replace` claims only the labels it is given, no longer those of the `runner.env` it replaces.

### API

- `agentiik-api namespace create NAME` creates a namespace and `namespace remove NAME` removes one holding no workflow, run or secret, where the API runs and with its database settings, until v0.3.0's routes. Both are audited as `namespace.create` and `namespace.delete`.

## v0.2.0, 2026-09-26

The first server release. An installation of the API, the controller, the bus and runners runs a pushed workflow as `agk run --local` runs it, and a test installation stood up from the checkout proves it in CI. Access control arrives in v0.3.0; until then only the operator token is allowed anything.

### Upgrading

- A run in flight across the upgrade from a v0.2.0 pre-release build reads a whole-number input as an int from its next pass, where it read a double, so drain runs before upgrading from one.

### Controller

- One active controller, elected by a PostgreSQL advisory lock, with a fencing counter on every write, so a partitioned former holder is refused. `Controller.Lead` checks the lock on every poll and ends the term with `controller.ErrLockLost` when it cannot.
- Woken by `NOTIFY` when the API writes a run, and sweeping on its own interval anyway. A pass that changes nothing writes nothing.
- Decides through the v0.1.0 evaluator. The run stores the evaluator's state, so failover is a resume, and envelopes are lifted into the object store and replaced by their digests.
- A task is published after its row commits and stamped once published; the sweep resends one whose message never went.
- Results are recorded idempotently, taken only from the runner its dispatch was bound to at redemption (`controller.ErrNotTheHolder`), and refused when they are not an ending, name no dispatch or name an object that is not their envelope (`controller.ErrNotAResult`). `controller.Answer` names outputs by digest.
- A task reads `running` while its container runs and `publishing` while its outputs go up. One that never reached a container is ended by the first runner to report it, with no exit code.
- Concurrency groups hold one started run, later ones queue in creation order, and `cancel_in_progress` cancels the running one first. `max_concurrent_tasks` holds tasks back rather than failing them.
- Retries wait out their backoff. A task carries its step's timeout as a deadline, the installation's ceiling where none bounds it (an hour by default), and a run past its root timeout is `timed_out`.
- A cancellation is acted on at the next pass, before admission; a run cancelled from `queued` has no `started_at`. A run that ends `cancelled` or `timed_out` ends every task not yet over the same way and stops every redeemed one.
- A task stopped as `superseded` or `sibling_failed`, or left in flight by a `merge: first` when its run ends, is written `cancelled` in the pass that sends the stop, freeing its slot at once.
- A `timed_out` or `cancelled` task keeps its container's exit code (137 or 143); a lost task and an ending no container reached have none (`graph.Result.NoExitCode`).
- A run that ends emits a completion carrying its identifier and state, preceded by a failure when it `failed` or `timed_out`.
- `retain` is resolved by the controller and recorded on the artifact reference.
- The sweep declares lost every task whose runner has said nothing of it for three heartbeat intervals. A lost task is requeued under the same idempotency key and a new `task_id` where the step is idempotent and `retry.on` names `lost`, spending no `retry.max` attempt, at most `max_requeues` times (3 by default); past it the step fails on the infrastructure's account. A redelivery or a wait on a full pool's queue is not a loss.
- A loss moves only the dispatch its `task_id` names, and is heard once however often it is reported.
- A requeue that reaches the host which already ended its key is answered with that ending.
- A step goes to the pool whose labels include every label of its `runs_on`, among the pools that accept the run's namespace, and one naming no label to `default`. No pool, or more than one, fails its pending shards with exit code 125 saying why (`graph.Result.Reason`). Resources are capped to the pool's ceilings.
- A run on a server starts with the workflow's `vars`.
- A number written without a fraction or an exponent is an int in an expression and any other a double, wherever it comes from, locally and on a server alike. `.inf` and `.nan` are refused.
- `agentiik-controller`: a static binary and a non-root image, linking no secret store. It stands by until it holds the lock, and asks PostgreSQL to probe its connections, so a controller cut off frees the lock within half a minute.
- The leading controller verifies the audit chain at the start of each term, from how far the last verification reached (`audit_verified`), and exports the log to `AGK_AUDIT_EXPORT_URL` as newline-delimited JSON, at least once. `agentiik-api audit-verify FILE` checks an export.
- Prometheus metrics on `AGK_METRICS_LISTEN`, behind a bearer token: dispatches, losses, retries, durations, latency, queue depth and runner slots. Only the leader reports, and a metric keeps at most 1,000 label sets. `controller.Options.Observer` is told each event.
- A run that ends is exported as one OpenTelemetry trace to `AGK_OTLP_ENDPOINT` (`internal/otlp`, standard library only). A collector that is down costs the spans and never a decision.
- `agentiik-api`, `agentiik-controller` and `agk-runner` end at a second SIGINT or SIGTERM (`internal/stopsignal`).

### State

- Package `db`: the schema, its migrations, and row level security on every namespaced table. A `Pool` has only three doors: `In` binds a namespace, `Installation` steps past it for a named reason, `Session` pins a connection. A superuser connection is refused.
- `db.Provision` applies the migrations under an advisory lock and creates, or brings back to shape, the `NOSUPERUSER NOBYPASSRLS` role the API and the controller connect as, with no superuser needed. It refuses a role that owns anything.
- Artifact and envelope references with reference counting, the three purges and the collector. A grant counts the input envelopes it names.
- `tasks` keeps one row per dispatch of a key, numbered by `requeue`, at most one of them not `lost`.
- A workflow, step, port or secret name is the `identifier` domain, up to 255 characters.
- `runs.cancel_requested_at` keeps the first request to cancel (`0018_cancel_requested.sql`).
- Runner pools hold their name, labels, namespaces and cpu, memory and pids ceilings (`0019_pool_shape.sql`); an installation is created with the pool `default`, which carries no label, accepts every namespace and has no ceiling (`0028_default_pool.sql`).
- A runner keeps its Ed25519 public key, its narrowed namespaces and its containment (`0021_runner_identity.sql`), its rotated credential until the new one is used (`0023_credential_rotation.sql`), and who drained or revoked it and when (`0025_revocation.sql`).
- A version keeps the digest each tag resolved to (`0020_version_images.sql`) and its tree, content-addressed, as a manifest of `path`, `sha256`, `size` and `mode`.
- Grants are kept, never replaced (`0014_grants_kept.sql`).
- `secret_declarations` keeps where each secret lives, never a value; `secret_values` keeps the built-in store's values sealed and versioned, refusing a delete, a truncate or a row moved.
- `artifacts.fetches_held_until` holds a fetch until it completes, so one that fails spends nothing.
- `tasks` may carry an exit code on a `timed_out` or `cancelled` row (`0027_stopped_exit_codes.sql`).
- The audit log, append-only and hash-chained, written in the act's transaction (`0030_audit_log.sql`); triggers refuse an update, a delete or a truncate to every role. Verification progress is kept in `audit_verified` (`0031_audit_verified.sql`).
- `agk.TriggerKind` has the seven kinds the documentation names, and `cron` is now `schedule`.
- `agk.LogURI` addresses a log by its task: `agk://log/<run>/<task>`.

### Bus

- Package `bus` over NATS JetStream: one WorkQueue stream with a subject per runner pool, results on a stream of their own, and stops on `agentiik.stops`, heard by every runner. The controller's half is `bus/control`, so a runner links `bus` without the controller or the database.
- Tasks, results, progress and stops travel as `wire.schema.json`, vendored with its fixtures. A task carries names and digests, not the input envelopes; a message nobody can read is taken off the queue and reported.
- A task is deduplicated on its `task_id`, and a result on its runner, `task_id` and ending, so a requeue always goes out and its ending always comes back.
- A result the controller could not record comes back after a pause, from a second doubling to a minute.
- A runner gets an hour-long credential from the API for its own pool: pull from that pool's consumer, acknowledge, publish on `agentiik.results.<runner>`, hear stops, reply under its own `bus.Inbox`, and nothing else. A revoked runner's credential only publishes and hears stops.
- A runner redeems a task's grant before acknowledging its message; `bus.AckWait` is a minute. A host that dies before redeeming leaves the message to another runner.
- `bus.NewInstallation` creates the operator and accounts and writes `accounts.conf`, `account.seed` and `control-plane.creds`, keeping the operator's seed nowhere. `bus.RenewControlPlane` renews the control plane's credential in place.
- `bus.Route` chooses a task's pool; `Bus.Depths` reads each pool's queue depth.
- A bus address in plaintext to anything but loopback is refused (`bus.ErrPlaintext`).

### Driver

- A redelivered task never starts its container twice, and a key that has completed on a host is never started there again: every ending is recorded under `.keys` in the work root for seven days, by reference and never a payload, and a later delivery is refused with a `driver.Completed` holding it.
- `Docker.Hold` writes a key down when a runner takes it and refuses one in flight (`driver.ErrTaskInFlight`) or completed; `Docker.Release`, `Docker.Recorded`, `Docker.Dispatched`, `Docker.Ended` and `Docker.Logged` read and amend the record.
- Each port's envelope is written to the store before the ending is recorded, and the terminal `driver.Event` names it by digest.
- `driver.WithSources` gives each task the store, secret source and tree its redemption answered.
- Every envelope is held to its limits before the first upload; a container that exited 0 with refused outputs is `failed` with exit code 121 (`driver.ExitContractBroken`), charged to the brick.
- A secret mount is one file directly under `/agk/secrets/`, and a name beginning with a dot is refused.
- Runner floors no line of `runner.toml` lifts: a remapped daemon needs `CAP_CHOWN`, `CAP_FOWNER` and `CAP_DAC_OVERRIDE` (`driver.ErrOwnershipCapabilities`), the secrets directory a tmpfs mounted `noexec,nosuid,nodev` (`driver.ErrSecretsTmpfsRequired`), the daemon a seccomp profile (`driver.ErrSeccompRequired`), and every image a digest (`driver.ErrImageNotByDigest`). They are read again after a daemon restart. `agk run --local` and `agk brick test` lift them.
- `driver.LoadPolicy` reads every setting of `/etc/agentiik/runner.toml` strictly, naming the line it refuses, and replaces `driver.ParseUsernsFloor`. A profile, label or cap the daemon would not honour is refused, and `[hooks]` runs nothing until v0.9.0.
- A task's `internal` network is isolated from the runner host through the gateway, and refused on a daemon older than Docker 28.0 (`driver.ErrInternalNotIsolated`). `Docker.Sweep` removes the networks a dead runner left.
- The pull and the manifest read are bounded by the task's deadline, and a pull refused for want of credentials says so.
- The terminal event carries the exit code, span, `cpu_seconds` and `max_rss_bytes` sampled from the daemon; a container gone before any sample reports neither.
- A task's log caps count standard error alone.
- `Docker.Pin` resolves a tag to the digest its registry serves, or `driver.ErrNotPushed`.
- A container is given `TRACEPARENT`, derived by `agk.RunID.Trace` and `agk.TaskSpan`, so the runner, the controller and `agk run --local` name the same trace.
- Empty task directories are swept from the work root and the secrets tmpfs after an hour.

### Runner

- `agk-runner`, one static binary and a `linux/amd64` and `linux/arm64` image (`build/runner.Dockerfile`), run as 65532 with the three capabilities as file capabilities, with the verbs `join`, `serve` and `version`.
- `join` generates the host's Ed25519 key, measures its capacity and joins, writing `/var/lib/agentiik/runner.key` and `/etc/agentiik/runner.env` (0600) once the API has answered. `--replace` replaces an existing identity.
- `serve` reads its settings from the environment or `runner.env`, read strictly and refused when anybody else can read it, and `/etc/agentiik/runner.toml`. It refuses root, a daemon below the floors and a `DOCKER_HOST` that is not a local unix socket, before any call to the API, and tells systemd `READY=1`.
- `serve` takes as many tasks as `AGK_RUNNER_CONCURRENCY` allows, and puts back one of a namespace `AGK_RUNNER_NAMESPACES` leaves out or that would exceed the host's memory and vCPU. Each is written down, redeemed, acknowledged, assembled (`runner.Assemble`), run and reported as `taskResult` (`runner.Carrier`).
- A redemption with no answer is tried again, from 1 s doubling to 30 s, until the deadline; one refused is reported with no container ran.
- A result is kept under `<work root>/.results` from before the task runs until the bus takes it, so a crash at any point loses none.
- A heartbeat every 10 s names every key the runner answers for; the answer's `cancel` stops tasks and `drain` stops taking. A 401 exits 3.
- Stops heard on `agentiik.stops` are sent to the driver once per key.
- Logs are shipped to `POST /api/v1/tasks/logs` while the container runs, a chunk a second or as soon as one is full (4,096 lines, 1 MiB), and resumed after a restart.
- `serve` renews its runner credential at two thirds of its window through `POST /api/v1/runners/rotate`, kept in `/var/lib/agentiik/credential`, and its bus credential at three quarters of its life on a second connection.
- A drained runner stays up, idle and reporting `draining`; a revoked one exits 3 once its results are published or its grace ends.

### Artifacts

- `artifact/granted` reads through the presigned GETs a redemption names and writes through the task's signed upload policy, refusing any other key without sending anything, and refusing `http` to anything but loopback.
- The store's refusals come back as `artifact.ErrNotSigned`, `artifact.ErrWrongDigest`, `artifact.ErrTooLarge` and `fs.ErrNotExist`, and no error names its URL.
- `artifact.Store.Describe` answers the entry `Put` would, writing nothing.

### API

- `agk.ReservedNamespaces` fixes the words the API routes on after `/api/v1/`, and a workflow may not use one as its namespace.
- Deny by default is structural: a route is registered with the permission it needs and the router checks it. A refusal at a namespace or a workflow is the same 404 as an absence; 403 is for the installation scope.
- A ninth permission, `secret:write`.
- Push a version: `agk push`'s commit, tree and `images` (each tag's digest). A version stores every file and image manifest, so it rebuilds with no tree and no registry, and pushing the same commit again changes nothing.
- Start a run (202), binding its inputs against the version's declaration as a local run does; an input refused is 422 with `error`, `input` and `rule`.
- `GET /api/v1/runs` and `GET /api/v1/{ns}/runs` list runs by the caller's `run:read` on each workflow, filtered by `namespace`, `workflow`, `state`, `since`, `until` and `limit`. `GET /api/v1/runs/{run}` reads one; its inputs need `run:read_data`.
- `POST /api/v1/runs/{run}/cancel` asks for a run to be cancelled, with `workflow:run`, answering 202.
- `GET /api/v1/runs/{run}/outputs/{name}`, `.../steps/{step}/outputs/{port}` and `.../steps/{step}/inputs/{port}` answer envelopes with `run:read_data`, 410 once purged.
- `GET /api/v1/artifacts/{uri}` redirects to a five-minute presigned URL, or serves the bytes where the artifact has a fetch budget.
- `GET /api/v1/runs/{run}/steps/{step}/logs` streams a step's log as server-sent events, history then live, resumable with `Last-Event-ID` (`0029_tasks_by_step.sql`). `POST /api/v1/tasks/logs` takes the runner's chunks (`0026_task_logs.sql`).
- Runner pools and single-use join tokens in the `runnerPool` shape, under `grant:manage`. A join speaks `runnerRegistration`, and a bad token always gets the same 401.
- Runner routes have a guard of their own, with one 401 for every bad credential. The heartbeat speaks `runnerHeartbeat` and answers `received_at`, `drain` and `cancel`.
- `POST /api/v1/runners/rotate` renews a runner credential, signed by the key the runner joined with.
- `POST /api/v1/runners/{runner}/drain` and `/revoke` take a `reason`. A revoked runner is heard for a grace (`AGK_REVOCATION_GRACE`, the task ceiling by default) to finish what it holds.
- Grants, join tokens and runner credentials carry 256 bits, are stored hashed and shown once. A grant is never replaced; a lost dispatch's grant, and every grant of a completed key, is refused.
- Redeeming a grant, with `task_id` and `idempotency_key`, answers in the `grantRedemption` shape: presigned URLs for input envelopes, artifacts and the version's tree, the secret values, and one signed upload policy. It binds the task only once it has an answer; a draining or revoked runner, or one whose pool or namespaces leave the task out, is refused, and what the installation can never answer is 422.
- A presigned URL names one method, one object, one run and an expiry. The built-in store serves objects at `/objects/{key...}` and takes uploads at `POST /objects/{namespace}`, hashing them as they arrive.
- `GET`, `PUT` and `DELETE /api/v1/{ns}/secrets/{name}` and the listing manage secret declarations (`builtin`, `env` or `vault`), never answering a value. A `builtin` `PUT` may carry the value. `env` is opt-in, under an `AGK_DEV_` prefix, and `vault` refused until its provider arrives.
- Bodies are read a token at a time under a cap per route, and a list past its count is 413; duplicate or case-varied fields, text that is not UTF-8 and U+0000 are refused.
- `POST /api/v1/bus/token` mints a runner's bus credential, and each pool's consumer is made ready as the pool is created.
- Manual triggers, cancellations, secret writes, pools, join tokens, drains and revocations are recorded in the audit log, never with a secret's value or a token.
- `agentiik-api`: a static binary and a non-root image, the one program that links the secret store, with `serve`, `migrate`, `bus-init`, `bus-credential` and `audit-verify`. Until v0.3.0, the token whose SHA-256 `AGK_OPERATOR_TOKEN_FILE` holds is allowed everything, and nobody else anything.

### Secrets

- Package `secret`, the built-in store: a fresh AES-256-GCM data key per value, wrapped by a master key from a file only the API user can open, rotating through a keyring.
- A workflow's `secrets` block is a list of names, `secrets: [billing]`; where a value lives is the namespace's declaration.
- `secret.Builtin`, `secret.Env` and `secret.Providers` read a secret through its declaration; `secret.Attach` wires them into the API.
- Each redemption reads its secrets again, so a rotated value arrives rotated.
- Tests hold that the API is the only component reading a secret value, through the redemption alone, and that no value reaches the database outside `secret_values`.

### Configuration

- Package `internal/config` reads each program's settings from its own `AGK_*` variables, and names every one missing or malformed.
- A secret is a file an `AGK_*_FILE` variable names, readable by its owner alone; a secret given as a value, a password in a URL, `PGPASSWORD` or `PGSSLPASSWORD` refuses the start. `config.Secret` prints as `[secret]`.
- No plaintext path: a database URL needs `sslmode` `verify-full`, `verify-ca` or `require` unless local, `AGK_BUS_URL` is `tls://` or `wss://`, and `AGK_PUBLIC_URL` is `https`. TLS 1.2 is the floor everywhere (`internal/tlsfloor`).
- `AGK_TLS_CERT_FILE` and `AGK_TLS_KEY_FILE` have the API and the metrics listener serve TLS themselves.
- The controller refuses `AGK_MASTER_KEY_FILE`, which is the API's alone.
- New settings: `AGK_MAX_REQUEUES`, `AGK_TASK_CEILING`, `AGK_REVOCATION_GRACE`, `AGK_ENV_PREFIXES`, `AGK_AUDIT_EXPORT_URL`, `AGK_AUDIT_EXPORT_TOKEN_FILE`, `AGK_METRICS_LISTEN`, `AGK_METRICS_TOKEN_FILE` and `AGK_OTLP_ENDPOINT`.

### Command line

- `agk push` sends a version and its commit's tree, read from git's objects, with every tag resolved to its digest. A dirty tree is refused unless `--allow-dirty`, and the credential comes from `AGENTIIK_TOKEN`.
- `agk run --namespace` starts a run of a pushed commit on an installation and follows it to its end as a local run does; `-o json` writes its output envelopes.
- `agk logs` follows a run's step logs, resuming a stream that drops without printing a line twice.
- `agk status` shows how a run stands.
- These four refuse an installation address that would carry `AGENTIIK_TOKEN` in plaintext.
- `agk run --local` ends a stopped `fail_fast` or `merge: first` sibling `cancelled`, as a server does: a `fail_fast` step hands out no further shard once one has failed.
- `agk validate` and `agk run --local` refuse a name longer than 255 characters.

### Tests

- The PostgreSQL, NATS and real-daemon tests run in CI, the remapped-daemon ones in a job of their own, and `internal/dbtest` gives each test its own database and role.
- Boundary tests for `driver`, `bus` and `agk-runner`, which links no database, controller, API or secret store and opens no inbound port.
- `internal/stoptest` plays one history through `agk run --local` and the controller, which must end it the same way.
- The real-daemon tests hold `network: internal` to what the kernel does.
- The runner image is built for both architectures and held to the floors.
- `e2e` stands up a test installation from the checkout: PostgreSQL, NATS, the API behind TLS, the controller, a registry and two runners on `docker:dind`. A runner killed mid-task is recovered by the other, and v0.1.0's milestone workflow gives the same envelopes there as under `agk run --local`. It runs under `AGENTIIK_E2E=1`.
- `cmd/agk/internal/diff` is now `internal/diff`.

## v0.1.2, 2026-09-13

Nothing changed here. The release is documentation: a [Get started](https://agentiik.github.io/docs#get-started) chapter and a recorded session of the command line on the home page.

## v0.1.1, 2026-09-13

This file. `v0.1.0` was tagged before its changelog was written, and a Go module tag cannot be moved once the checksum database has recorded it, so this release adds the description instead. A version's entry is now merged before its tag.

## v0.1.0, 2026-09-12

The first release in which a workflow runs.

- `agk run --local` runs a whole workflow against the local Docker daemon, with no controller, bus or database. `cmd/agk/milestone_test.go` runs one with a fan-out and a merge twice and gets the same envelopes.
- The libraries: `agk` (the vocabulary), `artifact` (the content-addressed store, per namespace), `brick` (the container's two edges), `schema` (JSON Schema 2020-12), `graph` (the evaluator) and `driver` (one task as one container).
- `agk validate`, `agk graph`, `agk run --local` and `agk brick test` work. `login`, `whoami`, `push`, `share`, `grants`, `logs` and `brick init` refuse and say why.
- `/agk/bin/agk`, a static helper for script steps: `agk items`, `agk emit` and `agk attach`.
- `network: egress` is refused until the proxy that enforces `egress.allow` exists.
- Where the host has no tmpfs, as on a Mac, a secret value is written into the task's working directory, removed with the container, and the run says so.
