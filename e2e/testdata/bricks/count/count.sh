#!/bin/sh
set -eu

# The smoke test's one brick: it reads the batch the runner fetched through a presigned URL and
# laid down at /agk/in/orders/envelope.json, and publishes one item and one artifact, which the
# runner stores through the upload policy it was handed. The number of items is the number of
# identities in the document, which the contract promises one of per item, so nothing here
# parses JSON.
items=$(grep -o '"id":' /agk/in/orders/envelope.json | wc -l | tr -d ' ')

report=/agk/out/files/report.txt
printf 'orders=%s\n' "$items" > "$report"
size=$(wc -c < "$report" | tr -d ' ')
digest=$(sha256sum "$report" | cut -d' ' -f1)

cat > /agk/out/ports/ok.json <<JSON
{"meta":{"run_id":"$AGK_RUN_ID","step":"$AGK_STEP","port":"ok","attempt":$AGK_ATTEMPT,"count":1,"produced_at":"1970-01-01T00:00:00Z"},"items":[{"id":"counted-$items","data":{"orders":$items},"files":[{"name":"report.txt","uri":"agk://run/$AGK_RUN_ID/$AGK_STEP/ok/report.txt","media_type":"text/plain","size":$size,"sha256":"$digest"}]}]}
JSON
