#!/bin/sh
set -eu

# A brick honouring the contract, as small as one can be: the batch is at
# /agk/in/<port>/envelope.json and what is left under /agk/out/ is collected.
#
# Nothing here parses JSON. A fixture brick should need as little as a brick can, and the
# number of items is the number of identities in the document, which the contract promises one
# of per item.
envelope=/agk/in/orders/envelope.json
items=0
if [ -f "$envelope" ]; then
  items=$(grep -o '"id":' "$envelope" | wc -l | tr -d ' ')
fi

if [ "$items" -eq 0 ]; then
  echo "the batch is empty, and this brick counts what it is given" >&2
  exit 2
fi

# The report is the artifact. Its bytes are derived from the batch, so two runs of one case
# produce one digest and a case whose input changed produces another.
report=/agk/out/files/report.txt
printf 'items_seen=%s\n' "$items" > "$report"
size=$(wc -c < "$report" | tr -d ' ')
digest=$(sha256sum "$report" | cut -d' ' -f1)

# The identity is derived from the payload rather than minted, which is what keeps a brick
# reproducible: an item holds its own identity so that a shard, a join and a replay can speak
# about it, and one minted on every pass has thrown that away.
cat > /agk/out/ports/ok.json <<JSON
{"meta":{"run_id":"$AGK_RUN_ID","step":"$AGK_STEP","port":"ok","attempt":$AGK_ATTEMPT,"count":1,"produced_at":"1970-01-01T00:00:00Z"},"items":[{"id":"counted-$items","data":{"items_seen":$items},"files":[{"name":"report.txt","uri":"agk://run/$AGK_RUN_ID/$AGK_STEP/ok/report.txt","media_type":"text/plain","size":$size,"sha256":"$digest"}]}]}
JSON
