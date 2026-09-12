#!/bin/sh
set -eu

# The working tree is bound read-only at /agk/repo: on a laptop what ran is what is on the
# disk, and a step that cannot see it is a local run that did not mount it.
test -f /agk/repo/agentiik.yaml

# The parameters are at /agk/params.json, which is where a brick reads them: the AGK_PARAM_
# variables are exported for a script step and a brick is given the file.
currency=$(sed -n 's/.*"currency"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /agk/params.json)
: "${currency:?the manifest declares currency required and /agk/params.json carries none}"

# Nothing here parses JSON. Each order carries one reference and one amount, so the two
# lists are read in order and paired by rank, which is the order the batch travels in.
envelope=/agk/in/orders/envelope.json
grep -o '"ref":"[^"]*"' "$envelope" | sed 's/^"ref":"//; s/"$//' > /tmp/refs
grep -o '"amount":[0-9][0-9]*' "$envelope" | sed 's/^"amount"://' > /tmp/amounts
awk 'NR==FNR { ref[FNR] = $0; next } { print ref[FNR], $0 }' /tmp/refs /tmp/amounts > /tmp/orders
count=$(wc -l < /tmp/orders | tr -d ' ')

# The identity is derived from the order and never minted: an item holds its own identity so
# that a shard, a merge and a second run can speak about it by the same name.
{
  printf '{"meta":{"run_id":"%s","step":"%s","port":"ok","attempt":%s,"count":%s,"produced_at":"1970-01-01T00:00:00Z"},"items":[' \
    "$AGK_RUN_ID" "$AGK_STEP" "$AGK_ATTEMPT" "$count"
  sep=""
  while read -r ref amount; do
    printf '%s{"id":"order-%s","data":{"amount":%s,"currency":"%s","ref":"%s"},"files":[]}' \
      "$sep" "$ref" "$amount" "$currency" "$ref"
    sep=","
  done < /tmp/orders
  printf ']}'
} > /agk/out/ports/ok.json
