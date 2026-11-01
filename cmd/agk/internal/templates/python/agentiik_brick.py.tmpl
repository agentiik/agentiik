"""Read and write the Agentiik brick contract.

The envelope a brick is given on each input port, the parameters and the run context, the
secrets mounted for it, one envelope per output port, the artifacts it attaches, and the exit
code that decides its step's verdict. Nothing here is required: the contract is files and an
exit code (https://agentiik.github.io/docs#brick), and a brick reading them itself is as much a
brick. What this saves is what is easy to get subtly wrong: the metadata the runner refuses an
envelope for, the size rules it refuses one for with exit code 121, and an exit code that is the
runner's rather than the brick's.

The Go and TypeScript helpers have the same surface, name for name, and the scenarios under
conformance/ hold all three to the same behaviour. The standard library is all this depends on.
"""

from __future__ import annotations

import dataclasses
import datetime
import hashlib
import json
import os
import sys
from typing import Any, Dict, List, Optional

__all__ = [
    "IN_DIR", "OUT_PORTS", "OUT_FILES", "PARAMS_FILE", "RUN_FILE", "SECRETS_DIR",
    "root", "stdin", "limits", "DEFAULT_LIMITS", "SizeLimits", "TooLarge",
    "read", "items", "emit", "attach", "params", "param", "context", "Run", "Shard", "secret",
    "FAILED", "TRANSIENT", "INVALID", "exit_code", "exit", "fail", "retry", "refuse", "segment",
]

# The paths the contract names. Constants and not configuration, because a brick whose paths
# came from somewhere would be a brick whose contract depends on how it was run.
IN_DIR = "/agk/in"
OUT_PORTS = "/agk/out/ports"
OUT_FILES = "/agk/out/files"
PARAMS_FILE = "/agk/params.json"
RUN_FILE = "/agk/run.json"
SECRETS_DIR = "/agk/secrets"

#: Prefixed to every path of the contract, and empty in a container. It exists for a test on the
#: machine a brick is built on, which should not need a container to be told where bytes go.
root = ""

#: Where read takes the envelope of the port in when no file was mounted for it.
stdin = sys.stdin


@dataclasses.dataclass
class SizeLimits:
    """The size rules an envelope is held to when the runner collects it. A breach fails the
    step with exit code 121, charged to the brick, so emit keeps to them itself."""

    inline_max: int = 262144
    envelope_max: int = 4194304
    max_items: int = 100000

    def or_default(self) -> "SizeLimits":
        d = DEFAULT_LIMITS
        return SizeLimits(
            self.inline_max if self.inline_max > 0 else d.inline_max,
            self.envelope_max if self.envelope_max > 0 else d.envelope_max,
            self.max_items if self.max_items > 0 else d.max_items,
        )


#: The documentation's defaults. The contract tells a brick no other, so a brick on an
#: installation that tightened them takes them as parameters and sets limits.
DEFAULT_LIMITS = SizeLimits()

#: The rules emit holds every envelope to. A field of zero is its default.
limits = SizeLimits()


class TooLarge(ValueError):
    """An envelope that breaks envelope_max_bytes or max_items with its heavy values spilled.
    Nothing more can be spilled, so it is the brick's to refuse: a step that meets it splits
    its batch upstream."""


def _at(path: str) -> str:
    return os.path.join(root, path.lstrip("/")) if root else path


def _check_name(name: str, what: str) -> None:
    if not name or name in (".", "..") or "/" in name or "\\" in name:
        raise ValueError(f"{what} {name!r} is not a name: one segment of a path, no separator, not . or ..")


class _Task:
    """What the environment says about this container: the metadata every envelope has to
    carry, refused by the runner when it disagrees."""

    def __init__(self) -> None:
        missing = [v for v in ("AGK_RUN_ID", "AGK_STEP", "AGK_ATTEMPT") if not os.environ.get(v)]
        if missing:
            raise ValueError(", ".join(missing) + " not set: the runner sets them in every container, and a brick run by hand is given them the same way")
        self.run_id = os.environ["AGK_RUN_ID"]
        self.step = os.environ["AGK_STEP"]
        attempt = os.environ["AGK_ATTEMPT"]
        try:
            self.attempt = int(attempt)
        except ValueError:
            self.attempt = 0
        if self.attempt < 1:
            raise ValueError(f"AGK_ATTEMPT is {attempt!r}, and an attempt is a whole number from 1")
        ports = os.environ.get("AGK_OUT_PORTS")
        self.ports = None if not ports else [p for p in ports.split(",") if p]

    def declared(self, port: str) -> bool:
        return self.ports is None or port in self.ports


def _encode(value: Any) -> bytes:
    """The JSON of a value as the engine writes it, Go's encoding/json: keys sorted, no space,
    <, > and & escaped, and the two line separators JavaScript cannot hold raw. It is what a
    value is measured and spilled as."""
    s = json.dumps(value, separators=(",", ":"), sort_keys=True, ensure_ascii=False)
    for raw, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        s = s.replace(raw, escaped)
    return s.encode("utf-8")


def read(port: str) -> Dict[str, Any]:
    """The envelope that arrived on an input port, from /agk/in/<port>/envelope.json.

    For the port in alone, a brick run by hand with no file mounted reads it from standard
    input, never from a terminal. A port nobody fed is an empty batch rather than an error: an
    edge above that published nothing is the ordinary case."""
    _check_name(port, "port")
    path = _at(f"{IN_DIR}/{port}/envelope.json")
    text: Optional[str] = None
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            text = f.read()
    elif port == "in" and stdin is not None and not (hasattr(stdin, "isatty") and stdin.isatty()):
        text = stdin.read()
    if text is None or not text.strip():
        return {"meta": {"port": port}, "items": []}
    try:
        e = json.loads(text)
    except json.JSONDecodeError as err:
        raise ValueError(f"the envelope on port {port} is not an envelope: {err}") from None
    if not isinstance(e, dict):
        raise ValueError(f"the envelope on port {port} is not an envelope: not an object")
    e.setdefault("items", [])
    for item in e["items"]:
        item.setdefault("data", {})
        item.setdefault("files", [])
    return e


def items(port: str) -> List[Dict[str, Any]]:
    """The items of an input port, which is what most bricks want of read."""
    return read(port)["items"]


def _attach(t: _Task, port: str, name: str, media_type: str, content: bytes) -> Dict[str, Any]:
    directory = _at(OUT_FILES)
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, name), "wb") as f:
        f.write(content)
    return {
        "name": name,
        "uri": f"agk://run/{t.run_id}/{t.step}/{port}/{name}",
        "media_type": media_type,
        "size": len(content),
        "sha256": hashlib.sha256(content).hexdigest(),
    }


def attach(port: str, name: str, media_type: str, content: bytes) -> Dict[str, Any]:
    """Writes an artifact under /agk/out/files/ and answers the files[] entry an item refers to
    it by, its size and digest computed here because the runner holds the item to them."""
    _check_name(name, "artifact")
    return _attach(_Task(), port, name, media_type, content)


def segment(s: str) -> str:
    """Files a string down to what one segment of a path and of a URI may hold, as the engine
    files the name of a value it spills."""
    out = []
    for ch in s:
        out.append(ch if ("a" <= ch <= "z" or "A" <= ch <= "Z" or "0" <= ch <= "9" or ch in "._-") else "_")
        if len(out) >= 64:
            break
    result = "".join(out)
    return "field" if not result.strip(".") else result


def _spilled_name(item_id: str, rank: int, field: str) -> str:
    base = segment(item_id or f"item-{rank}") + "-" + segment(field)
    name, n = base + ".json", 2
    while os.path.exists(os.path.join(_at(OUT_FILES), name)):
        name, n = f"{base}-{n}.json", n + 1
    return name


def emit(port: str, items: Optional[List[Dict[str, Any]]]) -> None:
    """Writes the envelope of one output port at /agk/out/ports/<port>.json.

    The metadata comes from the environment and count from the items. The envelope is held to
    the size rules first: a value of data above inline_max_bytes is spilled to an artifact named
    <item>-<field>.json, the engine's own name for it, and an envelope still above
    envelope_max_bytes, or holding more than max_items, raises TooLarge rather than leaving the
    runner to refuse it with 121."""
    _check_name(port, "port")
    t = _Task()
    if not t.declared(port):
        raise ValueError(f"port {port} is not one this step declares, which are {t.ports}: the runner refuses an envelope on any other")
    items = list(items or [])
    lim = limits.or_default()
    if len(items) > lim.max_items:
        raise TooLarge(f"the envelope breaks the size rules: port {port} holds {len(items)} items and max_items is {lim.max_items}")
    for rank, item in enumerate(items):
        data = item.setdefault("data", {})
        files = item.setdefault("files", [])
        for field in sorted(data):
            b = _encode(data[field])
            if len(b) <= lim.inline_max:
                continue
            files.append(_attach(t, port, _spilled_name(item.get("id", ""), rank, field), "application/json", b))
            del data[field]
    envelope = {
        "meta": {"run_id": t.run_id, "step": t.step, "port": port, "attempt": t.attempt, "count": len(items), "produced_at": "1970-01-01T00:00:00Z"},
        "items": [{"id": i.get("id", ""), "data": i["data"], "files": i["files"]} for i in items],
    }
    body = json.dumps(envelope, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    if len(body) > lim.envelope_max:
        raise TooLarge(f"the envelope breaks the size rules: the envelope on port {port} weighs {len(body)} bytes with its heavy values spilled, and envelope_max_bytes is {lim.envelope_max}")
    directory = _at(OUT_PORTS)
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, port + ".json"), "wb") as f:
        f.write(body + b"\n")


def params() -> Dict[str, Any]:
    """/agk/params.json: what the step chose. A step that supplied nothing leaves the file
    absent, which is an empty dict and not an error."""
    path = _at(PARAMS_FILE)
    if not os.path.exists(path):
        return {}
    with open(path, encoding="utf-8") as f:
        try:
            p = json.load(f)
        except json.JSONDecodeError as err:
            raise ValueError(f"the parameters at {PARAMS_FILE} are not a JSON object: {err}") from None
    if not isinstance(p, dict):
        raise ValueError(f"the parameters at {PARAMS_FILE} are not a JSON object")
    return p


_MISSING = object()


def param(name: str, default: Any = _MISSING) -> Any:
    """One parameter, or default where the step did not supply it, and KeyError without one. An
    explicit zero is returned as zero: a default applied over it is the bug a reader checks
    the manifest to rule out."""
    p = params()
    if name in p:
        return p[name]
    if default is _MISSING:
        raise KeyError(f"the step supplied no parameter {name}")
    return default


@dataclasses.dataclass
class Shard:
    """One shard of a fan-out: AGK_SHARD, 3/8 read as index 3 of count 8."""

    index: int
    count: int


@dataclasses.dataclass
class Run:
    """The run context: /agk/run.json and the AGK_* variables. A field the runner did not give
    is left empty rather than invented."""

    id: str = ""
    workflow: str = ""
    version: str = ""
    namespace: str = ""
    commit: str = ""
    trigger_kind: str = ""
    triggered_by: str = ""
    started_at: str = ""
    step: str = ""
    attempt: int = 0
    shard: Optional[Shard] = None
    deadline: Optional[datetime.datetime] = None
    repo: str = ""
    out_ports: List[str] = dataclasses.field(default_factory=list)
    traceparent: str = ""


def context() -> Run:
    """Reads the run context: what the run is from /agk/run.json, what this container is from
    the environment."""
    r = Run()
    path = _at(RUN_FILE)
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            doc = json.load(f)
        for key in ("id", "workflow", "namespace", "commit", "trigger_kind", "triggered_by", "started_at"):
            if isinstance(doc.get(key), str):
                setattr(r, key, doc[key])
    env = os.environ.get
    r.id = env("AGK_RUN_ID") or r.id
    if env("AGK_WORKFLOW"):
        r.workflow, _, r.version = env("AGK_WORKFLOW", "").partition("@")
    r.namespace = env("AGK_NAMESPACE") or r.namespace
    r.commit = env("AGK_COMMIT") or r.commit
    r.step = env("AGK_STEP", "")
    if env("AGK_ATTEMPT"):
        try:
            r.attempt = int(env("AGK_ATTEMPT", ""))
        except ValueError:
            raise ValueError(f"AGK_ATTEMPT is {env('AGK_ATTEMPT')!r}, and an attempt is a whole number") from None
    if env("AGK_SHARD"):
        index, sep, count = env("AGK_SHARD", "").partition("/")
        if not sep or not index.isdigit() or not count.isdigit():
            raise ValueError(f"AGK_SHARD is {env('AGK_SHARD')!r}, and a shard is written index/count")
        r.shard = Shard(int(index), int(count))
    if env("AGK_DEADLINE"):
        try:
            r.deadline = datetime.datetime.fromisoformat(env("AGK_DEADLINE", "").replace("Z", "+00:00"))
        except ValueError:
            raise ValueError(f"AGK_DEADLINE is {env('AGK_DEADLINE')!r}, and a deadline is RFC 3339") from None
    r.repo = env("AGK_REPO", "")
    r.out_ports = [p for p in env("AGK_OUT_PORTS", "").split(",") if p]
    r.traceparent = env("TRACEPARENT", "")
    return r


def secret(name: str) -> Optional[bytes]:
    """The secret mounted at /agk/secrets/<name>, or None where none is: a secret the manifest
    declares optional may be absent. Never the environment, where every process the brick
    starts would inherit it and the first to dump its environment would print it."""
    _check_name(name, "secret")
    path = _at(f"{SECRETS_DIR}/{name}")
    if not os.path.exists(path):
        return None
    with open(path, "rb") as f:
        return f.read()


#: An application failure, retried only where the step's retry.on says so.
FAILED = 1
#: A far side that may answer next time, retried by the step's policy.
TRANSIENT = 100
#: Input that will be as unusable on every attempt, never retried.
INVALID = 120


def exit_code(code: int) -> int:
    """The code exit leaves with: the code where a brick may leave by it, 1 to 120, and FAILED
    for anything else, which is the runner's or the runtime's."""
    return code if FAILED <= code <= INVALID else FAILED


def exit(code: int, message: str) -> None:  # noqa: A001, the contract's word
    """Writes one line on standard error, the task's log, and leaves with code mapped by
    exit_code. Never standard output, which no log keeps."""
    print(message, file=sys.stderr)
    if exit_code(code) != code:
        print(f"exit code {code} is not a brick's to leave with: 1 to 99 is a failure, 100 to 119 transient, 120 invalid input; leaving with {exit_code(code)}", file=sys.stderr)
    sys.exit(exit_code(code))


def fail(message: str) -> None:
    """Leaves with FAILED: the step failed, and is retried only where it says so."""
    exit(FAILED, message)


def retry(message: str) -> None:
    """Leaves with TRANSIENT: the far side was briefly unavailable. Never for a payload that
    will be as wrong next time."""
    exit(TRANSIENT, message)


def refuse(message: str) -> None:
    """Leaves with INVALID: what the step or the batch says is unusable on every attempt."""
    exit(INVALID, message)
