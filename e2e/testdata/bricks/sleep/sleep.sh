#!/bin/sh
set -eu

# The killed-runner test's one brick. It counts the items it was given, waits the seconds its
# step gives it, which for the middle step is long enough for the test to kill the runner holding
# it, and publishes one item naming its step. Nothing here parses JSON: the count is the number
# of identities in the envelope, one per item.
seconds=$(sed -n 's/.*"seconds"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' /agk/params.json)
: "${seconds:?the manifest declares seconds required and /agk/params.json carries none}"
items=$(grep -o '"id":' /agk/in/in/envelope.json | wc -l | tr -d ' ')

sleep "$seconds"

cat > /agk/out/ports/ok.json <<JSON
{"meta":{"run_id":"$AGK_RUN_ID","step":"$AGK_STEP","port":"ok","attempt":$AGK_ATTEMPT,"count":1,"produced_at":"1970-01-01T00:00:00Z"},"items":[{"id":"$AGK_STEP","data":{"items":$items,"slept":$seconds},"files":[]}]}
JSON
