#!/usr/bin/env bash
# Resumable upload test.
#
# Claim under test: a 2 GB+ video upload that is interrupted mid-transfer
# resumes and completes without re-uploading any already-completed part.
#
# What it does
#   1. Generates a 2.1 GiB video (scripts/make_sample_video.sh big).
#   2. Starts uploading it with the CLI client (the same engine the web app uses).
#   3. Watches the server's part list and kills the client with SIGKILL at ~50%,
#      then checks that it really died with PUTs still in flight.
#   4. Snapshots the bucket's part list (ETag and LastModified per part).
#   5. Restarts the client with the same command; it resumes.
#   6. Snapshots the bucket's part list again, before completing.
#   7. Asserts, from the server-side part lists:
#        - the kill really happened mid-transfer (0 < parts in bucket < total)
#        - the restarted client PUT none of the parts that were already there
#        - those parts have the same ETag and LastModified as before, i.e. the
#          bucket confirms they were never rewritten
#        - the restarted client PUT exactly the parts that were missing
#        - every part's ETag is the MD5 of the corresponding bytes of the file
#   8. Completes the upload; the api verifies the assembled object's size.
#
# Needs the api, the dev issuer and object storage running (`make dev`, or
# `make infra` plus the api and issuer), and ffmpeg, node, pnpm, jq, curl.
#
# Environment
#   API, ISSUER   origins (default http://localhost:8080 and http://localhost:8090)
#   SIZE_BYTES    file size (default 2254857830 = 2.1 GiB)
#   KILL_AT       fraction of parts after which to kill the client (default 0.5)
#   KEEP=1        keep the uploaded asset instead of deleting it afterwards
#   LOG           where to write the log (default docs/upload_resume_test.log)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
API="${API:-http://localhost:8080}"
ISSUER="${ISSUER:-http://localhost:8090}"
SIZE_BYTES="${SIZE_BYTES:-2254857830}"
KILL_AT="${KILL_AT:-0.5}"
LOG="${LOG:-$ROOT/docs/upload_resume_test.log}"
WORK="$ROOT/tmp/resume-test"
FILE="$WORK/big_$SIZE_BYTES.mov"
STATE="$WORK/upload-state.json"

mkdir -p "$WORK" "$(dirname "$LOG")"
: >"$LOG"
say() { echo "$*" | tee -a "$LOG"; }
fail() {
  say "FAIL: $*"
  exit 1
}

CLIENT_PID=""
cleanup() { [ -n "$CLIENT_PID" ] && kill -9 "$CLIENT_PID" 2>/dev/null || true; }
trap cleanup EXIT

say "== Rigforge resumable upload test =="
say "date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
say "api:     $API"
say "host:    $(uname -sm), $(sysctl -n machdep.cpu.brand_string 2>/dev/null || grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs)"

# ---- 1. the file --------------------------------------------------------
if [ ! -f "$FILE" ] || [ "$(wc -c <"$FILE" | tr -d ' ')" -lt "$SIZE_BYTES" ]; then
  "$ROOT/scripts/make_sample_video.sh" big "$FILE" "$SIZE_BYTES" | tee -a "$LOG"
fi
FILE_SIZE="$(wc -c <"$FILE" | tr -d ' ')"
say "file:    ${FILE#"$ROOT"/}"
say "size:    $FILE_SIZE bytes ($(awk -v s="$FILE_SIZE" 'BEGIN { printf "%.3f GiB, %.3f GB", s / 1073741824, s / 1e9 }'))"
[ "$FILE_SIZE" -gt 2147483648 ] || [ "$SIZE_BYTES" -lt 2147483648 ] || fail "file is not larger than 2 GiB"

# ---- 2. a token for a fresh test user -------------------------------------
USER_NAME="resume-test-$(date +%s)"
TOKEN="$(curl -fsS -X POST "$ISSUER/token" -H 'Content-Type: application/json' -d "{\"sub\":\"$USER_NAME\"}" | jq -r .access_token)"
[ -n "$TOKEN" ] && [ "$TOKEN" != null ] || fail "could not get a token from $ISSUER"
api() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }

# The client command. `node --import tsx` runs the TypeScript CLI in a single
# process (the `tsx` launcher would put a wrapper process in between).
rm -f "$STATE"
CLIENT=(node --import tsx scripts/upload-cli.ts "$FILE" --api "$API" --token "$TOKEN" --state "$STATE")

# ---- 3. first run, killed mid-transfer ------------------------------------
say ""
say "-- run 1: upload, to be killed at ~$(awk -v k="$KILL_AT" 'BEGIN { printf "%d", k * 100 }')% --"
# `exec` replaces the subshell with node, so $! is the uploader's own pid and
# SIGKILL hits the process that is doing the uploading. (An earlier version
# launched the client through a shell function; $! was then a wrapper shell,
# the kill left node running and the test passed without ever interrupting
# anything. The checks after the kill below exist because of that.)
(cd "$ROOT/web" && exec "${CLIENT[@]}" --no-complete) >"$WORK/run1.jsonl" 2>"$WORK/run1.err" &
CLIENT_PID=$!

UPLOAD_ID=""
for _ in $(seq 1 100); do
  UPLOAD_ID="$(jq -r 'select(.event == "plan") | .uploadId' "$WORK/run1.jsonl" 2>/dev/null | head -1 || true)"
  [ -n "$UPLOAD_ID" ] && break
  kill -0 "$CLIENT_PID" 2>/dev/null || fail "client exited before starting: $(cat "$WORK/run1.err")"
  sleep 0.1
done
[ -n "$UPLOAD_ID" ] || fail "client never reported an upload id"
PART_COUNT="$(jq -r 'select(.event == "plan") | .partCount' "$WORK/run1.jsonl" | head -1)"
PART_SIZE="$(jq -r 'select(.event == "plan") | .partSize' "$WORK/run1.jsonl" | head -1)"
KILL_AFTER="$(awk -v n="$PART_COUNT" -v k="$KILL_AT" 'BEGIN { printf "%d", n * k }')"
say "upload:  $UPLOAD_ID"
say "plan:    $PART_COUNT parts of $PART_SIZE bytes ($((PART_SIZE / 1048576)) MiB), last part $((FILE_SIZE - (PART_COUNT - 1) * PART_SIZE)) bytes"

# Poll the SERVER (not the client's own output) for how many parts it has.
while :; do
  RECORDED="$(api "$API/api/uploads/$UPLOAD_ID" | jq '.parts | length')"
  if [ "$RECORDED" -ge "$KILL_AFTER" ]; then
    kill -9 "$CLIENT_PID"
    break
  fi
  kill -0 "$CLIENT_PID" 2>/dev/null || fail "client finished before it could be killed (only $RECORDED parts recorded); is the link too fast? lower KILL_AT"
  sleep 0.05
done
KILL_STATUS=0
wait "$CLIENT_PID" 2>/dev/null || KILL_STATUS=$?
say "sent SIGKILL to the client (pid $CLIENT_PID) when the server had recorded $RECORDED of $PART_COUNT parts"

# Prove the client is really dead and really was interrupted: it exited with
# 137 (128 + SIGKILL), no client process is left, it never reported a result,
# and some of the PUTs it started never finished.
[ "$KILL_STATUS" -eq 137 ] || fail "client exit status was $KILL_STATUS, expected 137 (killed by SIGKILL)"
! kill -0 "$CLIENT_PID" 2>/dev/null || fail "client process $CLIENT_PID is still alive"
! pgrep -f "upload-cli.ts $FILE" >/dev/null || fail "an upload client for this file is still running"
CLIENT_PID=""
[ "$(jq -s '[.[] | select(.event == "result")] | length' "$WORK/run1.jsonl")" -eq 0 ] || fail "run 1 finished; it was not interrupted"
RUN1_STARTED="$(jq -c -s '[.[] | select(.event == "put_start") | .part] | sort' "$WORK/run1.jsonl")"
RUN1_FINISHED="$(jq -c -s '[.[] | select(.event == "put_done") | .part] | sort' "$WORK/run1.jsonl")"
IN_FLIGHT="$(jq -n -c --argjson s "$RUN1_STARTED" --argjson f "$RUN1_FINISHED" '$s - $f')"
say "client exit status: $KILL_STATUS (killed by signal 9); no client process left"
say "  PUTs the client had finished:        $(jq -n --argjson f "$RUN1_FINISHED" '$f | length') parts"
say "  PUTs in flight when it was killed:   $IN_FLIGHT"
[ "$IN_FLIGHT" != "[]" ] || fail "no PUT was in flight when the client was killed"
RUN1_LINES="$(wc -l <"$WORK/run1.jsonl")"
sleep 2 # let the bucket settle any request that was in flight

# ---- 4. server-side snapshot before the restart -----------------------------
api "$API/api/uploads/$UPLOAD_ID?storage=true" >"$WORK/before.json"
BEFORE_PARTS="$(jq -c '[.storageParts[].partNumber]' "$WORK/before.json")"
BEFORE_COUNT="$(jq '.storageParts | length' "$WORK/before.json")"
BEFORE_RECORDED="$(jq '.parts | length' "$WORK/before.json")"
BEFORE_BYTES="$(jq '[.storageParts[].size] | add' "$WORK/before.json")"
say "bucket part list before restart: $BEFORE_COUNT of $PART_COUNT parts, $BEFORE_BYTES bytes ($BEFORE_RECORDED recorded by the api)"
say "  parts: $BEFORE_PARTS"
[ "$BEFORE_COUNT" -gt 0 ] && [ "$BEFORE_COUNT" -lt "$PART_COUNT" ] || fail "upload was not interrupted mid-transfer"

# ---- 5. second run: same command, resumes -----------------------------------
say ""
say "-- run 2: restart the client --"
(cd "$ROOT/web" && exec "${CLIENT[@]}" --no-complete) >"$WORK/run2.jsonl" 2>"$WORK/run2.err" ||
  fail "resumed client failed: $(cat "$WORK/run2.err" "$WORK/run2.jsonl" | tail -5)"
# The killed client wrote nothing more while run 2 was uploading.
[ "$(wc -l <"$WORK/run1.jsonl")" -eq "$RUN1_LINES" ] || fail "run 1 was still writing its log after being killed"
RESUMED_ID="$(jq -r 'select(.event == "plan") | .uploadId' "$WORK/run2.jsonl")"
[ "$RESUMED_ID" = "$UPLOAD_ID" ] || fail "client started a new upload ($RESUMED_ID) instead of resuming $UPLOAD_ID"
SKIPPED="$(jq -c 'select(.event == "result") | .skippedParts' "$WORK/run2.jsonl")"
RUN2_PUT="$(jq -c -s '[.[] | select(.event == "put_start") | .part] | sort' "$WORK/run2.jsonl")"
say "client resumed upload $RESUMED_ID"
say "  parts found on the server and skipped: $SKIPPED"
say "  parts PUT by run 2:                    $RUN2_PUT"

# ---- 6. server-side snapshot after the restart, before completing -----------
api "$API/api/uploads/$UPLOAD_ID?storage=true" >"$WORK/after.json"
AFTER_COUNT="$(jq '.storageParts | length' "$WORK/after.json")"
AFTER_BYTES="$(jq '[.storageParts[].size] | add' "$WORK/after.json")"
say "bucket part list after restart:  $AFTER_COUNT of $PART_COUNT parts, $AFTER_BYTES bytes"

# ---- 7. assertions ---------------------------------------------------------
say ""
say "-- checks --"

# (a) Nothing that was in the bucket before the restart was PUT again.
RESENT="$(jq -n -c --argjson before "$BEFORE_PARTS" --argjson put "$RUN2_PUT" '[$put[] | select(. as $p | $before | index($p))]')"
RESENT_COUNT="$(jq -n --argjson r "$RESENT" '$r | length')"
say "parts completed before the kill:              $BEFORE_COUNT"
say "of those, re-sent after the restart:          $RESENT_COUNT  $RESENT"
[ "$RESENT_COUNT" -eq 0 ] || fail "completed parts were re-uploaded"

# (b) The bucket agrees: those parts carry the same ETag and LastModified.
REWRITTEN="$(jq -n -c --slurpfile b "$WORK/before.json" --slurpfile a "$WORK/after.json" '
  [$b[0].storageParts[] as $old
   | ($a[0].storageParts[] | select(.partNumber == $old.partNumber)) as $new
   | select($new.etag != $old.etag or $new.lastModified != $old.lastModified)
   | $old.partNumber]')"
say "parts whose ETag or LastModified changed:     $(jq -n --argjson r "$REWRITTEN" '$r | length')  $REWRITTEN"
[ "$REWRITTEN" = "[]" ] || fail "the bucket shows completed parts were rewritten"

# (c) Run 2 sent exactly the parts that were missing.
EXPECTED="$(jq -n -c --argjson before "$BEFORE_PARTS" --argjson n "$PART_COUNT" '[range(1; $n + 1)] - $before')"
say "parts missing before the restart:             $(jq -n --argjson e "$EXPECTED" '$e | length')"
say "parts sent after the restart:                 $(jq -n --argjson p "$RUN2_PUT" '$p | length')"
[ "$RUN2_PUT" = "$EXPECTED" ] || fail "run 2 sent $RUN2_PUT, expected exactly $EXPECTED"

# (d) The bucket now holds every part, and together they are the whole file.
[ "$AFTER_COUNT" -eq "$PART_COUNT" ] || fail "bucket has $AFTER_COUNT parts, expected $PART_COUNT"
[ "$AFTER_BYTES" -eq "$FILE_SIZE" ] || fail "parts add up to $AFTER_BYTES bytes, file is $FILE_SIZE"
say "bytes in the bucket vs file size:             $AFTER_BYTES = $FILE_SIZE"

# (e) Content: on S3 and MinIO a part's ETag is the MD5 of its bytes. Compare
#     each with the MD5 of the same byte range of the local file. (R2 uses
#     opaque part ETags, so the comparison is skipped there.)
MD5_CHECK="$(python3 - "$FILE" "$PART_SIZE" "$WORK/after.json" <<'PY'
import hashlib, json, re, sys
path, part_size, snapshot = sys.argv[1], int(sys.argv[2]), sys.argv[3]
etags = {p["partNumber"]: p["etag"] for p in json.load(open(snapshot))["storageParts"]}
if not all(re.fullmatch(r"[0-9a-f]{32}", e) for e in etags.values()):
    print("skipped (this bucket does not use MD5 part ETags)")
    sys.exit(0)
mismatched = []
with open(path, "rb") as f:
    for number in sorted(etags):
        if hashlib.md5(f.read(part_size)).hexdigest() != etags[number]:
            mismatched.append(number)
print(f"{len(etags) - len(mismatched)} of {len(etags)} match" + (f"; MISMATCH in parts {mismatched}" if mismatched else ""))
sys.exit(1 if mismatched else 0)
PY
)" || fail "part content differs from the file: $MD5_CHECK"
say "part ETag = MD5 of the file's bytes:          $MD5_CHECK"

# ---- 8. complete ------------------------------------------------------------
say ""
say "-- complete --"
(cd "$ROOT/web" && exec "${CLIENT[@]}" --character none --name "resume test") >"$WORK/run3.jsonl" 2>"$WORK/run3.err" ||
  fail "complete failed: $(cat "$WORK/run3.err" "$WORK/run3.jsonl" | tail -5)"
RUN3_PUT="$(jq -c -s '[.[] | select(.event == "put_start") | .part]' "$WORK/run3.jsonl")"
ASSET_ID="$(jq -r 'select(.event == "result") | .assetId' "$WORK/run3.jsonl")"
[ "$RUN3_PUT" = "[]" ] || fail "completing sent parts again: $RUN3_PUT"
[ -n "$ASSET_ID" ] && [ "$ASSET_ID" != null ] || fail "complete returned no asset"
STORED_SIZE="$(api "$API/api/assets/$ASSET_ID" | jq .sourceSize)"
say "upload completed: asset $ASSET_ID, source object $STORED_SIZE bytes (size verified by the api against the bucket)"
[ "$STORED_SIZE" -eq "$FILE_SIZE" ] || fail "asset size $STORED_SIZE differs from the file size $FILE_SIZE"

if [ "${KEEP:-0}" != 1 ]; then
  api -X DELETE "$API/api/assets/$ASSET_ID" >/dev/null
  say "cleaned up: asset deleted"
fi

say ""
say "RESULT: PASS"
say "  file size:                         $FILE_SIZE bytes ($(awk -v s="$FILE_SIZE" 'BEGIN { printf "%.2f GiB", s / 1073741824 }'))"
say "  parts in total:                    $PART_COUNT"
say "  parts completed before the kill:   $BEFORE_COUNT"
say "  parts re-sent after the restart:   $RESENT_COUNT"
say "  parts sent after the restart:      $(jq -n --argjson p "$RUN2_PUT" '$p | length')"
