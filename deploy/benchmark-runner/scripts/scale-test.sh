#!/usr/bin/env bash
# scale-test.sh measures how a Capp's autoscaling responds to load and records
# what it observed. It runs load.js in the background and watches the Ready pods
# until k6 exits. Not reaching a pod count, or a slow cold start, is not a
# failure.
#
# Exit codes: 0 when the run passed, 1 when it broke: pods could not be read,
# k6 did not complete, no request succeeded, or the summary could not be imported.
set -uo pipefail

SCRIPTS_DIR="${SCRIPTS_DIR:-/scripts}"
# shellcheck source=lib.sh
source "$SCRIPTS_DIR/lib.sh"

CAPP_NAME="${CAPP_NAME:?CAPP_NAME must be set}"
CAPP_NAMESPACE="${CAPP_NAMESPACE:?CAPP_NAMESPACE must be set}"
CAPP_CLUSTER="${CAPP_CLUSTER:?CAPP_CLUSTER must be set}"
CAPP_REVISION="${CAPP_REVISION:?CAPP_REVISION must be set}"
RUN_ID="${RUN_ID:?RUN_ID must be set}"
TARGET_URL="${TARGET_URL:?TARGET_URL must be set}"
VM_IMPORT_URL="${VM_IMPORT_URL:?VM_IMPORT_URL must be set}"
BENCHMARK_SA_TOKEN="${BENCHMARK_SA_TOKEN:?BENCHMARK_SA_TOKEN must be set}"
TARGET_PATH="${TARGET_PATH:-/}"
ZERO_WAIT_SECONDS="${ZERO_WAIT_SECONDS:-180}"
SCALE_POLL_SECONDS="${SCALE_POLL_SECONDS:-2}"

HEADERS_JSON="${TARGET_HEADERS:-}"
[ -n "$HEADERS_JSON" ] || HEADERS_JSON='{}'
mapfile -t CURL_HEADERS < <(jq -r 'to_entries[] | "-H", "\(.key): \(.value)"' <<< "$HEADERS_JSON")

start=$(date +%s)
summary=$(mktemp)
trap 'rm -f "$summary"' EXIT

# kc runs kubectl as the configured benchmark ServiceAccount.
kc() {
  kubectl --token="$BENCHMARK_SA_TOKEN" "$@"
}

# finish <result> <reason> [stat=value...] writes the run's one summary and exits.
# A run that failed, or whose summary could not be imported, exits 1.
finish() {
  local result="$1" reason="$2"
  shift 2
  local push_status=0
  write_summary scale "$result" "$reason" "$@" || push_status=$?
  if [ "$push_status" -ne 0 ]; then
    echo "could not import the scale summary" >&2
    exit 1
  fi
  if [ "$result" = failed ]; then
    echo "scale run failed: $reason" >&2
    exit 1
  fi
  exit 0
}

# ready_pods hides kubectl errors, so permission to read pods is checked up front.
if ! kc get pods -n "$CAPP_NAMESPACE" -l serving.knative.dev/service="$CAPP_NAME" >/dev/null; then
  echo "ERROR: cannot list pods in namespace $CAPP_NAMESPACE" >&2
  finish failed "could not read pods in the namespace" start_seconds="$start" end_seconds="$(date +%s)"
fi

# ready_pods: count of Ready pods backing the Knative service.
ready_pods() {
  kc get pods -n "$CAPP_NAMESPACE" \
    -l serving.knative.dev/service="$CAPP_NAME" \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
    | grep -c "True" || true
}

# Scale-to-zero is owned by the Capp (minReplicas=0). If the Capp is not at zero,
# the cold-start metric is skipped rather than measuring a warm pod.
echo "==> Confirming $CAPP_NAME is at zero pods (wait up to ${ZERO_WAIT_SECONDS}s)..."
zero_deadline=$(( $(date +%s) + ZERO_WAIT_SECONDS ))
while [ "$(ready_pods)" -ne 0 ] && [ "$(date +%s)" -lt "$zero_deadline" ]; do
  sleep 5
done
PODS_AT_START=$(ready_pods)
echo "    ready pods now: $PODS_AT_START"

COLD=""
if [ "$PODS_AT_START" -eq 0 ]; then
  echo "==> Measuring cold-start TTFB (scale from zero)..."
  COLD=$(curl -o /dev/null -s -w '%{time_starttransfer}' --max-time 180 "${CURL_HEADERS[@]}" "${TARGET_URL}${TARGET_PATH}" || echo "")
  echo "    cold_start_seconds: ${COLD:-<failed>}"
else
  echo "    WARN: not at zero pods - skipping cold-start (would measure a warm pod)"
fi

# The watch state. MAX_PODS and TIME_TO_MAX are the most Ready pods seen and when
# they were first seen; SCALE_OUT is when more than one pod was first seen. A
# reading of zero pods records no time, so a run that never reached a pod has none.
MAX_PODS=0
TIME_TO_MAX=""
SCALE_OUT=""

# poll_pods records one reading of the Ready pods, relative to LOAD_START.
poll_pods() {
  local n now
  n=$(ready_pods)
  if [ "$n" -gt 0 ] && { [ -z "$TIME_TO_MAX" ] || [ "$n" -gt "$MAX_PODS" ]; }; then
    now=$(date +%s.%N)
    MAX_PODS=$n
    TIME_TO_MAX=$(jq -n "$now - $LOAD_START")
    echo "    $n ready pods at ${TIME_TO_MAX}s"
  fi
  if [ -z "$SCALE_OUT" ] && [ "$n" -gt 1 ]; then
    now=$(date +%s.%N)
    SCALE_OUT=$(jq -n "$now - $LOAD_START")
    echo "    scale-out began: ${SCALE_OUT}s ($n pods)"
  fi
}

echo "==> Starting sustained load..."
LOAD_START=$(date +%s.%N)
k6 run --quiet --summary-export="$summary" "$SCRIPTS_DIR/load.js" >/dev/null 2>&1 &
K6_PID=$!

echo "==> Watching the autoscaler until k6 exits..."
while kill -0 "$K6_PID" 2>/dev/null; do
  poll_pods
  sleep "$SCALE_POLL_SECONDS"
done
poll_pods
wait "$K6_PID"
k6_exit=$?
end=$(date +%s)

stats=(start_seconds="$start" end_seconds="$end" max_pods="$MAX_PODS")
if [ "$MAX_PODS" -gt 0 ]; then
  stats+=(time_to_max_pods_seconds="$TIME_TO_MAX")
fi
if [ -n "$SCALE_OUT" ]; then
  stats+=(scale_out_seconds="$SCALE_OUT")
fi
if [ -n "$COLD" ]; then
  stats+=(cold_start_seconds="$COLD")
fi

echo "==> Results: max_pods=$MAX_PODS cold_start=${COLD:-NaN} scale_out=${SCALE_OUT:-NaN} time_to_max=${TIME_TO_MAX:-NaN}"

if [ "$k6_exit" -ne 0 ]; then
  finish failed "the benchmark could not complete (k6 exited with $k6_exit)" "${stats[@]}"
fi

# A request succeeded when k6 counted at least one and the failure ratio is below 1.
# Both values are required: a missing key cannot show that a request succeeded.
succeeded=false
if values=$(jq -r 'def required(f): f | if . == null then error("missing") else . end;
  "\(required(.metrics.http_req_failed.value)) \(required(.metrics.http_reqs.count))"' "$summary" 2>/dev/null); then
  read -r failed count <<< "$values"
  if [ "$(jq -rn --argjson f "$failed" --argjson c "$count" '$f < 1 and $c > 0' 2>/dev/null)" = true ]; then
    succeeded=true
  fi
fi
if [ "$succeeded" != true ]; then
  finish failed "no request succeeded" "${stats[@]}"
fi

finish passed "" "${stats[@]}"
