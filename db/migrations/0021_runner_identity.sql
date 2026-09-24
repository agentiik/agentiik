-- A runner as wire.schema.json writes the join, $defs/runnerRegistration: the machine's public key,
-- the namespaces the host narrows itself to, and what it can prove about how its containers are
-- contained, beside the labels, capacity, architecture and agent version it already sent.
--
-- No installation holds a runner yet, since v0.1.0 had no server, so what is required is added as
-- required rather than filled in for rows that cannot exist.

alter table runners
  -- The runner identifier is minted by the API in the grammar the wire prints for a runner,
  -- lowercase words joined by hyphens, because it is the runner field of every result, a subject
  -- token on the bus and a segment of a URL, and the result reader refuses anything else. A
  -- lowercase ULID is one such word.
  add constraint runners_id_check
    check (id ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$'),

  -- The public half of the Ed25519 keypair the host generated at join, as its 32 bytes. "This
  -- key is the runner's identity across restarts, which is what makes a reimaged host a new
  -- runner that joins again": a rotation is signed with the private half, so a stolen credential
  -- without the key cannot be renewed. Kept as the key rather than as the PEM it arrived in, so
  -- that one key has one spelling here whatever line breaks the host wrote it with. Not unique:
  -- a host that joins twice is two runners, and the first record stays for the audit log.
  add column public_key bytea not null check (octet_length(public_key) = 32),

  -- The host's own narrowing, AGK_RUNNER_NAMESPACES, sent only when the host accepts fewer
  -- namespaces than its pool. 0010 dropped a column of this name because it widened: a machine
  -- declared which namespaces it served. This one only narrows, and the join refuses a namespace
  -- the pool does not accept. Null narrows nothing, which is the ordinary case, and an empty list
  -- is refused, because "a runner accepting no namespace is a runner that can never be handed a
  -- task".
  add column accepted_namespaces namespace_name[]
    check (cardinality(accepted_namespaces) > 0),

  -- What the host reported of its containment at join: the runtime the daemon creates its
  -- containers with, and whether the daemon remaps container root. Reported "so that an operator
  -- can see from the console what a fleet is actually running on", and optional, as the wire has
  -- it. Both or neither, since the wire sends them as one block with both fields required.
  add column containment_runtime text
    check (containment_runtime ~ '^[a-z][a-z0-9_.-]*$'),
  add column userns_remap boolean,
  add constraint runners_containment_whole
    check ((containment_runtime is null) = (userns_remap is null)),

  -- vCPU as the wire counts it, which sets a least and no most. A bigint for the reason a pool's
  -- pids ceiling is one: an integer would refuse a count the wire accepts, and the API would
  -- answer that as a failure of its own.
  alter column cpu type bigint;
