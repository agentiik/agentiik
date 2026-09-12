#!/bin/sh
# The smallest brick: read the envelope both ways it is given, report the environment,
# write one item on out and one file beside it.
set -e

stdin_bytes=$(cat | wc -c | tr -d ' ')
mounted=""
if [ -f /agk/in/in/envelope.json ]; then
  mounted=$(cat /agk/in/in/envelope.json)
fi

# Everything a brick is promised, reported to standard error so the log carries it.
echo "probe: stdin carried ${stdin_bytes} bytes" >&2
echo "probe: whoami=$(id -u):$(id -g)" >&2
echo "probe: AGK_RUN_ID=${AGK_RUN_ID} AGK_WORKFLOW=${AGK_WORKFLOW} AGK_NAMESPACE=${AGK_NAMESPACE}" >&2
echo "probe: AGK_STEP=${AGK_STEP} AGK_ATTEMPT=${AGK_ATTEMPT} AGK_SHARD=${AGK_SHARD}" >&2
echo "probe: AGK_DEADLINE=${AGK_DEADLINE} AGK_REPO=${AGK_REPO} AGK_COMMIT=${AGK_COMMIT}" >&2
echo "probe: AGK_OUT_PORTS=${AGK_OUT_PORTS}" >&2
echo "probe: repo carries $(ls /agk/repo | tr '\n' ' ')" >&2
echo "probe: params=$(tr -d '\n ' < /agk/params.json)" >&2
echo "probe: run=$(tr -d '\n ' < /agk/run.json)" >&2
if [ -f /agk/secrets/bearer ]; then
  echo "probe: the secret reads $(cat /agk/secrets/bearer)" >&2
fi
echo "probe: mounted envelope $(echo "${mounted}" | tr -d '\n ' | cut -c1-400)" >&2
echo "probe: the root filesystem is $(touch /probe-write 2>/dev/null && echo writable || echo 'read only')" >&2

printf 'a file the brick wrote\n' > /agk/out/files/report.txt
size=$(wc -c < /agk/out/files/report.txt | tr -d ' ')
sha=$(sha256sum /agk/out/files/report.txt | cut -d' ' -f1)

cat > /agk/out/ports/out.json <<JSON
{"meta":{"run_id":"${AGK_RUN_ID}","step":"${AGK_STEP}","port":"out","attempt":${AGK_ATTEMPT},"count":1,"produced_at":"2026-01-01T00:00:00Z"},"items":[{"id":"01JMZ8V1P9C5XQ7K2N4D6F8H0A","data":{"stdin_bytes":${stdin_bytes},"uid":"$(id -u)","step":"${AGK_STEP}"},"files":[{"name":"report.txt","uri":"agk://run/${AGK_RUN_ID}/${AGK_STEP}/out/report.txt","media_type":"text/plain","size":${size},"sha256":"${sha}"}]}]}
JSON
