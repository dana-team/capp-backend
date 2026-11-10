#!/usr/bin/env bash
# run-k6.sh <latency|throughput> runs one k6 script and records its result.
#
# Exit codes: 0 when the run passed, 1 when it failed or could not complete.
# A run fails when the error ratio reaches the limit or k6 exits 99 (a threshold
# was crossed). Speed never fails a run. A run that could not complete (no
# summary, or any k6 exit other than 0 or 99) is recorded as failed with its
# exit code.
set -uo pipefail

SCRIPTS_DIR="${SCRIPTS_DIR:-/scripts}"
# shellcheck source=lib.sh
source "$SCRIPTS_DIR/lib.sh"

type="${1:?usage: run-k6.sh <latency|throughput>}"
case "$type" in
  latency) limit=1 ;;
  throughput) limit=5 ;;
  *) echo "unknown benchmark type $type" >&2; exit 2 ;;
esac
: "${RUN_ID:?RUN_ID must be set}"
: "${CAPP_NAME:?CAPP_NAME must be set}"
: "${CAPP_NAMESPACE:?CAPP_NAMESPACE must be set}"
: "${CAPP_CLUSTER:?CAPP_CLUSTER must be set}"
: "${CAPP_REVISION:?CAPP_REVISION must be set}"
: "${TARGET_URL:?TARGET_URL must be set}"
: "${VM_IMPORT_URL:?VM_IMPORT_URL must be set}"
TARGET_PATH="${TARGET_PATH:-/}"

summary=$(mktemp)
trap 'rm -f "$summary"' EXIT

start=$(date +%s)
k6 run --out experimental-prometheus-rw --summary-trend-stats='p(50),p(95),p(99)' \
  --summary-export="$summary" "$SCRIPTS_DIR/$type.js"
k6_exit=$?
end=$(date +%s)

# fmt prints a number with up to 12 significant digits, so jq's float output reads cleanly.
fmt() { printf '%.12g' "$1"; }

# broke records a run that did not complete and exits. Its reason is the
# optional argument, or the k6 exit code when none is given.
broke() {
  local detail="${1:-k6 exited with $k6_exit}"
  local reason="the benchmark could not complete ($detail)"
  write_summary "$type" failed "$reason" start_seconds="$start" end_seconds="$end" || true
  echo "benchmark $type did not complete: $detail" >&2
  exit 1
}

if [ ! -s "$summary" ] || { [ "$k6_exit" -ne 0 ] && [ "$k6_exit" -ne 99 ]; }; then
  broke
fi

# k6 reports durations in ms; the summary records seconds. Every value is
# required: a missing key means the summary does not match what this script
# reads, so the run cannot be judged. A value of 0 is valid.
if ! values=$(jq -r 'def required(f): f | if . == null then error("missing") else . end;
  [
    (required(.metrics.http_req_duration["p(50)"]) / 1000),
    (required(.metrics.http_req_duration["p(95)"]) / 1000),
    (required(.metrics.http_req_duration["p(99)"]) / 1000),
    required(.metrics.http_req_failed.value),
    required(.metrics.http_reqs.rate)
  ] | @tsv' "$summary" 2>/dev/null); then
  broke "k6 summary was missing expected values"
fi
read -r p50 p95 p99 error_ratio rps <<< "$values"

over_limit=$(jq -rn --argjson r "$error_ratio" --argjson l "$limit" '$r * 100 >= $l')
result=passed
reason=""
if [ "$k6_exit" -eq 99 ] || [ "$over_limit" = "true" ]; then
  result=failed
  pct=$(jq -rn --argjson r "$error_ratio" '$r * 100')
  printf -v pct '%.1f' "$pct"
  reason="$pct% of requests failed, the limit is $limit%"
fi

stats=(start_seconds="$start" end_seconds="$end"
  p50_seconds="$(fmt "$p50")" p95_seconds="$(fmt "$p95")" p99_seconds="$(fmt "$p99")"
  error_ratio="$(fmt "$error_ratio")")
if [ "$type" = throughput ]; then
  stats+=(requests_per_second="$(fmt "$rps")")
fi

push_status=0
write_summary "$type" "$result" "$reason" "${stats[@]}" || push_status=$?
if [ "$result" = failed ] || [ "$push_status" -ne 0 ]; then
  [ "$push_status" -ne 0 ] && echo "could not import the $type summary" >&2
  exit 1
fi
exit 0
