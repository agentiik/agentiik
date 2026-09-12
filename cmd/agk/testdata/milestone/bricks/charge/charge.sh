#!/bin/sh
set -eu

# One shard is one item, so the first reference in the envelope is this container's order.
envelope=/agk/in/in/envelope.json
ref=$(grep -o '"ref":"[^"]*"' "$envelope" | head -n 1 | sed 's/^"ref":"//; s/"$//')
amount=$(grep -o '"amount":[0-9][0-9]*' "$envelope" | head -n 1 | sed 's/^"amount"://')
currency=$(grep -o '"currency":"[^"]*"' "$envelope" | head -n 1 | sed 's/^"currency":"//; s/"$//')

# The secret is a file bound read-only at /agk/secrets/<name>, never a variable. Its length
# travels so that the mount can be asserted on, and its value never does.
if echo x >> /agk/secrets/billing_api 2>/dev/null; then
  echo "the secret is bound read-write" >&2
  exit 1
fi
signed=$(wc -c < /agk/secrets/billing_api | tr -d ' ')

# The receipt is the artifact, and its bytes are derived from the order alone: two runs over
# the same order therefore store one object and the envelopes carry one digest.
receipt=/agk/out/files/receipt-$ref.txt
printf 'ref=%s\namount=%s\ncurrency=%s\n' "$ref" "$amount" "$currency" > "$receipt"
size=$(wc -c < "$receipt" | tr -d ' ')
digest=$(sha256sum "$receipt" | cut -d' ' -f1)

# An order of nothing leaves on rejected, which is what makes a second port worth having:
# the merge below reads both and the run does not fail for it.
if [ "$amount" -gt 0 ]; then
  port=ok
  id=charge-$ref
  data="{\"charged\":$amount,\"currency\":\"$currency\",\"ref\":\"$ref\",\"signed_bytes\":$signed,\"state\":\"charged\"}"
else
  port=rejected
  id=reject-$ref
  data="{\"currency\":\"$currency\",\"reason\":\"amount_not_positive\",\"ref\":\"$ref\",\"signed_bytes\":$signed,\"state\":\"rejected\"}"
fi

cat > "/agk/out/ports/$port.json" <<JSON
{"meta":{"run_id":"$AGK_RUN_ID","step":"$AGK_STEP","port":"$port","attempt":$AGK_ATTEMPT,"count":1,"produced_at":"1970-01-01T00:00:00Z"},"items":[{"id":"$id","data":$data,"files":[{"name":"receipt-$ref.txt","uri":"agk://run/$AGK_RUN_ID/$AGK_STEP/$port/receipt-$ref.txt","media_type":"text/plain","size":$size,"sha256":"$digest"}]}]}
JSON
