#!/bin/sh
set -eu

# The parameters are at /agk/params.json, which is where a brick reads them.
cycle=$(sed -n 's/.*"cycle"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /agk/params.json)
: "${cycle:?the manifest declares cycle required and /agk/params.json carries none}"

envelope=/agk/in/charges/envelope.json

# What arrived is both upstream ports concatenated, so both states are in one batch.
charged=$(grep -o '"state":"charged"' "$envelope" | wc -l | tr -d ' ')
rejected=$(grep -o '"state":"rejected"' "$envelope" | wc -l | tr -d ' ')
total=$(grep -o '"charged":[0-9][0-9]*' "$envelope" | sed 's/^"charged"://' | awk '{ sum += $1 } END { printf "%d", sum + 0 }')

# The artifacts the fan-out attached are laid down beside the envelope they arrived in, which
# is the whole round trip: stored by one step and fetched for the next, with the container
# never reaching the store.
receipts=$(ls /agk/in/charges | grep -c '^receipt-' || true)

summary=/agk/out/files/summary.txt
printf 'cycle=%s\ncharged=%s\nrejected=%s\ntotal=%s\nreceipts=%s\n' \
  "$cycle" "$charged" "$rejected" "$total" "$receipts" > "$summary"
size=$(wc -c < "$summary" | tr -d ' ')
digest=$(sha256sum "$summary" | cut -d' ' -f1)

cat > /agk/out/ports/ok.json <<JSON
{"meta":{"run_id":"$AGK_RUN_ID","step":"$AGK_STEP","port":"ok","attempt":$AGK_ATTEMPT,"count":1,"produced_at":"1970-01-01T00:00:00Z"},"items":[{"id":"invoice-$cycle","data":{"charged_count":$charged,"charged_total":$total,"cycle":"$cycle","receipts_seen":$receipts,"rejected_count":$rejected},"files":[{"name":"summary.txt","uri":"agk://run/$AGK_RUN_ID/$AGK_STEP/ok/summary.txt","media_type":"text/plain","size":$size,"sha256":"$digest"}]}]}
JSON
