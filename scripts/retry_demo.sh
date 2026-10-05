#!/usr/bin/env bash
# Demonstrates the job queue's failure paths on the kind deployment, for real:
#
#   A. Transient failure -> retry with backoff -> success.
#      Object storage is taken away while a worker starts the job; the attempt
#      fails, the job waits out its 1-minute backoff in jobs:delayed, storage
#      comes back, and attempt 2 succeeds.
#
#   B. Permanent failure -> dead-letter stream, no retries.
#      A video with nobody in it cannot be fixed by retrying; it goes straight
#      to jobs:dead with the reason.
#
# Needs the kind deployment (make deploy-kind), node/pnpm, ffmpeg, jq, curl.
# The transcript is kept in docs/retry_demo.log.
#
# Environment
#   BASE      ingress URL (default http://localhost:8880)
#   KUBECTL   kubectl command (default "kubectl --context kind-rigforge")
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${BASE:-http://localhost:8880}"
KUBECTL="${KUBECTL:-kubectl --context kind-rigforge}"
LOG="${LOG:-$ROOT/docs/retry_demo.log}"
WORK="$ROOT/tmp/retry-demo"
mkdir -p "$WORK"
: >"$LOG"

say() { echo "$*" | tee -a "$LOG"; }
k() { $KUBECTL -n rigforge "$@"; }
redis() { k exec deploy/redis -- redis-cli "$@"; }

TOKEN="$(curl -fsS -X POST "$BASE/issuer/token" -H 'Content-Type: application/json' -d "{\"sub\":\"retry-demo-$(date +%s)\"}" | jq -r .access_token)"
api() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }

# Upload a file with the CLI client and print the job id.
upload() {
  rm -f "$1.upload.json"
  (cd "$ROOT/web" && node --import tsx scripts/upload-cli.ts "$1" --api "$BASE" --token "$TOKEN" --name "$2") |
    jq -r 'select(.event == "result") | .jobId'
}

# Print every change of a job's state until it is done or failed.
follow() {
  local last="" started
  started="$(date +%s)"
  while :; do
    local job line
    job="$(api "$BASE/api/jobs/$1")"
    line="$(echo "$job" | jq -r '"status=\(.status) attempt=\(.attempt)/\(.maxAttempts) retryAt=\(.retryAt // "-") error=\(.error // "-")"')"
    if [ "$line" != "$last" ]; then
      say "  t+$(printf '%3d' $(($(date +%s) - started)))s  $line"
      last="$line"
    fi
    case "$(echo "$job" | jq -r .status)" in done | failed) return ;; esac
    sleep 1
  done
}

say "== Rigforge retry and dead-letter demo =="
say "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)   target: $BASE"

# ---- A. transient failure, retried --------------------------------------------
say ""
say "-- A. transient failure: storage disappears, the job is retried and recovers --"
say "stopping the workers so the job waits in the stream"
k scale deployment/worker --replicas=0 >/dev/null
k wait --for=delete pod -l app=worker --timeout=180s >/dev/null
cp "$ROOT/docs/samples/power_jump.mp4" "$WORK/retry.mp4"
JOB="$(upload "$WORK/retry.mp4" "Retry demo")"
say "uploaded the sample clip; job $JOB"
say "queue: XLEN jobs = $(redis XLEN jobs), delayed = $(redis ZCARD jobs:delayed), dead = $(redis XLEN jobs:dead)"

say "taking object storage away (scaling MinIO to 0) and starting the workers"
k scale deployment/minio --replicas=0 >/dev/null
k wait --for=delete pod -l app=minio --timeout=120s >/dev/null # really gone, not just terminating
k scale deployment/worker --replicas=2 >/dev/null

# Bring storage back as soon as the first attempt has failed and the retry is
# scheduled, i.e. while the job is waiting out its backoff.
(
  until [ "$(api "$BASE/api/jobs/$JOB" | jq -r .attempt)" = 2 ]; do sleep 1; done
  echo "  (storage restored: scaling MinIO back to 1)" | tee -a "$LOG"
  k scale deployment/minio --replicas=1 >/dev/null
) &
follow "$JOB"
wait
say "queue: XLEN jobs = $(redis XLEN jobs), delayed = $(redis ZCARD jobs:delayed), dead = $(redis XLEN jobs:dead)"
[ "$(api "$BASE/api/jobs/$JOB" | jq -r '"\(.status) \(.attempt)"')" = "done 2" ] || {
  say "FAIL: expected the job to finish on attempt 2"
  exit 1
}
say "result: the job failed once, waited 60 s, and succeeded on attempt 2"

# ---- B. permanent failure, dead-lettered ----------------------------------------
say ""
say "-- B. permanent failure: a video with nobody in it goes to the dead-letter stream --"
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=640x360:rate=30" -t 2 -pix_fmt yuv420p "$WORK/nobody.mp4"
DEAD_BEFORE="$(redis XLEN jobs:dead)"
JOB="$(upload "$WORK/nobody.mp4" "Nobody here")"
say "uploaded a 2-second test pattern; job $JOB"
follow "$JOB"
say "dead-letter stream: XLEN jobs:dead went from $DEAD_BEFORE to $(redis XLEN jobs:dead); newest entry:"
redis XREVRANGE jobs:dead + - COUNT 1 | sed 's/^/    /' | tee -a "$LOG"
[ "$(api "$BASE/api/jobs/$JOB" | jq -r '"\(.status) \(.attempt)"')" = "failed 1" ] || {
  say "FAIL: expected the job to fail permanently on attempt 1"
  exit 1
}
say "result: failed on attempt 1 without retries; the error is on the job row and in jobs:dead"

say ""
say "RESULT: PASS"
