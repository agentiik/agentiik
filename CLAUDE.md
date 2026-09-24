# Agentiik

A multi-user workflow engine: every brick is an ephemeral OCI container, every workflow is a git repository whose `agentiik.yaml` describes a directed acyclic graph, and one installation serves many users through namespaces, sharing and named roles.

This file is the same in all thirteen repositories of the organisation. Its source is `agentiik/.github/CLAUDE.md` and it is distributed from there, so edit it in that repository and never in a copy.

## The documentation is the authority

<https://agentiik.github.io/docs> is where the product is decided. Code implements it; it does not define it. When the two disagree, the documentation is right until a pull request changes it, and changing it is a decision that belongs to François.

Two rules follow, and they are the ones most often broken:

**Read it before writing.** Every chapter has an anchor, and the roadmap links each task to the section that specifies it. A task with no section behind it means either the documentation is missing a page or the task is not needed; both are worth raising rather than guessing.

**Update it in the same breath.** Building something almost always settles a question the documentation left open, and that answer belongs on the site before it is forgotten. A pull request that changes behaviour the documentation describes is incomplete until the page is fixed too, in its own pull request against `agentiik.github.io`, linked to the work that prompted it.

## How the work is tracked

<https://agentiik.github.io/docs/roadmap> lists 620 tasks in sixty groups across ten milestones, ordered by dependency and carrying no dates. The milestones are the releases: `v0.4.0` in the roadmap is the tag `v0.4.0` on every repository.

Each group is an issue of type Feature in the repository it names, 61 of them, because the v1.0.0 group covering disclosure, licensing and the release itself is filed as two. Its tasks become sub-issues of type Task when the group starts, which is what starting a group means: the plan changes by rewriting, and rewriting a paragraph costs nothing while rewriting fifty open issues costs an afternoon.

Everything sits on the organisation project, <https://github.com/orgs/agentiik/projects/1>, where `Release`, `Starts after`, `Spec` and `Section` answer what a per-repository milestone cannot.

## One version, everywhere

Every repository carries the same version, tagged at the same moment, released together, even where nothing changed. The sentence a person says out loud is "I am running Agentiik 1.4.0", and it has to name one thing. Per-repository versions turn that into a compatibility matrix, and a matrix nobody keeps up to date is wrong more often than right.

`0.y.z` promises nothing, which is where the project is. `1.0.0` is the point at which breaking the interface becomes a major version rather than an ordinary release.

## Git

**Never commit to `main`.** Branch, then open a pull request, including for documentation and one-line fixes.

**Commit messages are a single short title.** No body, no attribution, no tool mentioned. Author them as `François Rousselet <francois.rousselet@rslt.fr>`. The reasoning goes in the pull request description, where it belongs.

**Pull requests open as drafts** while the work is in progress, and are marked ready when it is finished and its checks are green. Add François as assignee then, since GitHub refuses a review request from the author of a pull request and these are authored by his token. A pull request that is not a draft is one that should be merged.

**Link the work.** A pull request opens with `Closes #12, closes #13`, plus `Part of #N` for the group issue, which stays open while tasks remain. It carries the milestone of its release and sits on the project with its fields set. Unlinked work leaves issues to be closed by hand and a board that says nothing is happening while something is.

**A release is written before it is tagged.** Every repository carries a `CHANGELOG.md`, and a version's entry is merged before the tag is placed, never after. A tag that has to be moved to pick up its own changelog often cannot be moved at all: `v0.1.0` was tagged first, and within minutes `sum.golang.org` had recorded the commit of `agentiik` and `bricks` in a public append-only log, so moving those tags would have made `go get` serve the old code from the proxy for ever and a direct fetch fail with a checksum mismatch that reads as a supply-chain attack. `v0.1.1` exists because of that ordering.

## Writing

**English throughout**, including comments, identifiers and commit messages.

**No em dash.** Not in prose, code, comments, error strings or commit messages. Repunctuate rather than substitute: a parenthetical pair becomes commas or brackets, a break before a consequence becomes a colon, an afterthought becomes a semicolon or a new sentence, and where none of those carry the sentence, recast it. An en dash is not a substitute either.

**Do not hard-wrap prose.** A paragraph is one line and the renderer decides where it breaks. Fixed columns read badly everywhere but a terminal, and editing one sentence forces a rewrap that makes the diff larger than the change.

**Identifiers are never translated or prettified.** A permission is `run:read_data`, a port is `rejected`, a state is `succeeded`, a quota is `max_concurrent_tasks`. The same string in the YAML, the API, the HCL, the schema and the prose. This rule has been broken four times in nine lines of example HCL; check it rather than trusting it.

**Sentence case in headings, and chapters are never numbered.**

**Prose explains why.** A limit with no rationale, a refusal with no reason, a default nobody justifies: each is a gap. Decisions a reader should be able to argue with go in a `<div class="decision">` block on the site, or a comment saying why beside the code.

## The repositories

| | |
| --- | --- |
| `agentiik` | The core in Go: graph evaluator, container driver, controller, HTTP API, runner, `agk`. AGPL-3.0-or-later. |
| `schemas` | The workflow, brick and envelope schemas, and the OpenAPI document. Every keyword carries a `description` and `examples`, and the build fails without them, because the language reference and `workflow.language` are generated from here. |
| `bricks`, `brick-sdk` | The standard catalog and the optional helpers. A brick is not a derivative work of the engine. |
| `design`, `console`, `ios`, `android` | The tokens and the four clients. |
| `deploy`, `terraform-provider-agentiik` | Compose stacks and the provider. |
| `homebrew-tap` | The Homebrew formulae: `agk` alone, and the server programs with it. |
| `agentiik.github.io` | This documentation and the roadmap. |
| `.github` | What every repository shares, including this file. |

The licence split is deliberate and set out in `LICENSING.md`: copyleft where the server lives, permissive everywhere a third party has to embed something.

## Working in Go

The module is `github.com/agentiik/agentiik`, Go 1.27. `go build ./...`, `go vet ./...`, `go test ./... -race` and `gofmt -l .` all run clean before a pull request is marked ready, and the workflow runs the same four.

The graph evaluator and the container driver stay importable libraries with no server, no bus and no database behind them. This is the documentation's constraint rather than a preference: it is what makes `agk run --local` the same code path as a server run rather than a second implementation that drifts. A test holds that boundary.

The standard library comes first. A dependency records its reason in `go.mod`, where the next reader will look for it.

## Things that have bitten

**YAML 1.1 resolves `on` as a boolean**, and the trigger block of a workflow is spelled `on`. A naive parser hands the engine `true` where it expects a key. Load workflow documents as YAML 1.2, as `tools/check.py` in the schemas repository does and says why.

**A script that reads across repositories cannot use the default `GITHUB_TOKEN`**, which reaches only the repository it runs in. A tool that treats an empty read as "nothing is done" will erase what it cannot see; the roadmap marker did exactly that once and now refuses a partial read.
