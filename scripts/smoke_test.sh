#!/usr/bin/env bash
# Post-deploy smoke test: push the sample clip through a running deployment
# using nothing but curl, and check that a motion asset comes out.
#
#   scripts/smoke_test.sh [BASE_URL]
#
#   BASE_URL   where the web app / ingress listens
#              (default http://localhost:8880, the kind deployment;
#               use http://localhost:8081 for docker-compose)
#
# It walks the same protocol the browser does: get a token from the dev
# issuer, create an upload, PUT the file to the presigned URL, report the
# ETag, complete, follow the job to the end, then fetch the outputs through
# their presigned URLs.
#
# Environment
#   SAMPLE       video to upload (default docs/samples/power_jump.mp4)
#   CONNECT_TO   passed to curl --connect-to, for running where the public
#                host names do not resolve as they do in a browser
#   TIMEOUT      seconds to wait for the job (default 300)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${1:-http://localhost:8880}"
SAMPLE="${SAMPLE:-$ROOT/docs/samples/power_jump.mp4}"
TIMEOUT="${TIMEOUT:-300}"
EXTRA=()
if [ -n "${CONNECT_TO:-}" ]; then
  for mapping in $CONNECT_TO; do EXTRA+=(--connect-to "$mapping"); done
fi

# All requests go through c(). The odd-looking expansion is for bash 3.2 (the
# macOS default), where "${EXTRA[@]}" on an empty array trips `set -u`.
c() { curl ${EXTRA[@]+"${EXTRA[@]}"} "$@"; }
req() { c -fsS "$@"; }
# Tiny JSON reader: the value of the FIRST "name": in the input. Enough for
# flat responses, and it keeps the script free of a jq dependency so it can
# run inside a cluster node.
field() { grep -o "\"$1\":\"\{0,1\}[^\",}]*" | head -1 | sed 's/^[^:]*://; s/^"//'; }
fail() {
  echo "SMOKE TEST FAILED: $*" >&2
  exit 1
}

echo "target: $BASE"
[ "$(req "$BASE/api/health" | field status)" = ok ] || fail "api health check"

TOKEN="$(req -X POST "$BASE/issuer/token" -H 'Content-Type: application/json' -d "{\"sub\":\"smoke-$(date +%s)\"}" | field access_token)"
[ -n "$TOKEN" ] || fail "no token from the issuer"
AUTH=(-H "Authorization: Bearer $TOKEN")
# Without a token the api must refuse.
[ "$(c -s -o /dev/null -w '%{http_code}' "$BASE/api/assets")" = 401 ] || fail "api did not require a token"

SIZE="$(wc -c <"$SAMPLE" | tr -d ' ')"
CREATED="$(req "${AUTH[@]}" -X POST "$BASE/api/uploads" -H 'Content-Type: application/json' \
  -d "{\"filename\":\"$(basename "$SAMPLE")\",\"size\":$SIZE,\"contentType\":\"video/mp4\"}")"
UPLOAD_ID="$(echo "$CREATED" | field uploadId)"
[ -n "$UPLOAD_ID" ] || fail "create upload: $CREATED"
echo "upload:  $UPLOAD_ID ($SIZE bytes, $(echo "$CREATED" | field partCount) part)"

PART_URL="$(req "${AUTH[@]}" -X POST "$BASE/api/uploads/$UPLOAD_ID/parts" -H 'Content-Type: application/json' \
  -d '{"partNumbers":[1]}' | sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | sed 's/\\u0026/\&/g')"
[ -n "$PART_URL" ] || fail "no presigned URL"
# The bytes go straight to the bucket; the api never sees them.
ETAG="$(c -fsS -X PUT --data-binary "@$SAMPLE" -D - -o /dev/null "$PART_URL" |
  tr -d '\r' | sed -n 's/^[Ee][Tt]ag: *//p' | tr -d '"')"
[ -n "$ETAG" ] || fail "the bucket returned no ETag"
req "${AUTH[@]}" -X PUT "$BASE/api/uploads/$UPLOAD_ID/parts/1" -H 'Content-Type: application/json' -d "{\"etag\":\"$ETAG\"}" >/dev/null

DONE="$(req "${AUTH[@]}" -X POST "$BASE/api/uploads/$UPLOAD_ID/complete" -H 'Content-Type: application/json' -d '{"name":"Smoke test"}')"
ASSET_ID="$(echo "$DONE" | field assetId)"
JOB_ID="$(echo "$DONE" | field jobId)"
[ -n "$JOB_ID" ] || fail "complete: $DONE"
echo "asset:   $ASSET_ID"
echo "job:     $JOB_ID"

# Follow the job. (The web app uses the SSE stream; polling is enough here.)
STARTED="$(date +%s)"
LAST=""
while :; do
  JOB="$(req "${AUTH[@]}" "$BASE/api/jobs/$JOB_ID")"
  STATUS="$(echo "$JOB" | field status)"
  if [ "$STATUS" != "$LAST" ]; then
    echo "  $(($(date +%s) - STARTED))s  $STATUS"
    LAST="$STATUS"
  fi
  case "$STATUS" in
  done) break ;;
  failed) fail "job failed: $(echo "$JOB" | field error)" ;;
  esac
  [ $(($(date +%s) - STARTED)) -lt "$TIMEOUT" ] || fail "job still $STATUS after ${TIMEOUT}s"
  sleep 1
done

ASSET="$(req "${AUTH[@]}" "$BASE/api/assets/$ASSET_ID")"
[ "$(echo "$ASSET" | field status)" = ready ] || fail "asset is not ready: $ASSET"
echo "asset is ready: $(echo "$ASSET" | field frameCount) frames, $(echo "$ASSET" | field durationSeconds) s"

# The outputs are downloadable through short-lived presigned URLs.
for format in glb bvh; do
  LINK="$(req "${AUTH[@]}" "$BASE/api/assets/$ASSET_ID/export?format=$format" |
    sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | sed 's/\\u0026/\&/g')"
  # A range request for the first 9 bytes: enough to recognise the format.
  # Binary bytes: under a UTF-8 locale BSD tr rejects them ("Illegal byte
  # sequence"), so this one call works on bytes.
  HEAD="$(c -fsS -r 0-8 "$LINK" | LC_ALL=C tr -d '\0')"
  BYTES="$(c -fsS -o /dev/null -w '%{size_download}' "$LINK")"
  case "$format" in
  glb) [ "${HEAD:0:4}" = glTF ] || fail "motion.glb does not start with the glTF magic" ;;
  bvh) [ "$HEAD" = HIERARCHY ] || fail "motion.bvh does not start with HIERARCHY" ;;
  esac
  echo "motion.$format: $BYTES bytes"
done

req "${AUTH[@]}" -X DELETE "$BASE/api/assets/$ASSET_ID" >/dev/null
echo "cleaned up: asset deleted"
echo "SMOKE TEST PASSED"
