#!/usr/bin/env bash
# scale-test.sh measures Knative autoscaling for a Capp and pushes the results
# to VictoriaMetrics' Prometheus import endpoint.
#
# Metrics produced:
#   capp_cold_start_seconds    - TTFB of the first request after a scale-to-zero
#   capp_scale_delay_seconds   - load start -> scale-out past the first pod
#   capp_time_to_scale_seconds - load start -> TARGET_PODS pods Ready
#   capp_scale_test_success    - 1 if TARGET_PODS was reached within the timeout, else 0
#
# Exits non-zero when pods cannot be read or TARGET_PODS is not reached, so the
# Job reports failed.
set -uo pipefail

CAPP_NAME="${CAPP_NAME:?CAPP_NAME must be set}"
CAPP_NAMESPACE="${CAPP_NAMESPACE:?CAPP_NAMESPACE must be set}"
CAPP_CLUSTER="${CAPP_CLUSTER:?CAPP_CLUSTER must be set}"
TARGET_URL="${TARGET_URL:?TARGET_URL must be set}"
BENCHMARK_SA_TOKEN="${BENCHMARK_SA_TOKEN:?BENCHMARK_SA_TOKEN must be set}"
TARGET_PATH="${TARGET_PATH:-/}"
TARGET_PODS="${TARGET_PODS:-5}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-180}"
ZERO_WAIT_SECONDS="${ZERO_WAIT_SECONDS:-180}"
VM_IMPORT_URLS="${VM_IMPORT_URLS:-}"

HEADERS_JSON="${TARGET_HEADERS:-}"
[ -n "$HEADERS_JSON" ] || HEADERS_JSON='{}'
mapfile -t CURL_HEADERS < <(jq -r 'to_entries[] | "-H", "\(.key): \(.value)"' <<< "$HEADERS_JSON")

# kc runs kubectl as the configured benchmark ServiceAccount.
kc() {
  kubectl --token="$BENCHMARK_SA_TOKEN" "$@"
}

# ready_pods hides kubectl errors, so permission to read pods is checked up front.
if ! kc get pods -n "$CAPP_NAMESPACE" -l serving.knative.dev/service="$CAPP_NAME" >/dev/null; then
  echo "ERROR: cannot list pods in namespace $CAPP_NAMESPACE" >&2
  exit 1
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
  COLD=$(curl -o /dev/null -s -w '%{time_starttransfer}' --max-time "$TIMEOUT_SECONDS" "${CURL_HEADERS[@]}" "${TARGET_URL}${TARGET_PATH}" || echo "")
  echo "    cold_start_seconds: ${COLD:-<failed>}"
else
  echo "    WARN: not at zero pods - skipping cold-start (would measure a warm pod)"
fi

echo "==> Starting sustained load..."
LOAD_START=$(date +%s.%N)
k6 run --quiet /scripts/load.js >/dev/null 2>&1 &
K6_PID=$!

echo "==> Watching autoscaler scale-out (target=$TARGET_PODS pods, timeout=${TIMEOUT_SECONDS}s)..."
SCALE_DELAY=""
TIME_TO_SCALE=""
SUCCESS=0
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  n=$(ready_pods)
  now=$(date +%s.%N)
  if [ -z "$SCALE_DELAY" ] && [ "$n" -gt 1 ]; then
    SCALE_DELAY=$(jq -n "$now - $LOAD_START")
    echo "    scale-out began: ${SCALE_DELAY}s ($n pods)"
  fi
  if [ "$n" -ge "$TARGET_PODS" ]; then
    TIME_TO_SCALE=$(jq -n "$now - $LOAD_START")
    SUCCESS=1
    echo "    reached $TARGET_PODS pods in ${TIME_TO_SCALE}s"
    break
  fi
  sleep 2
done

kill "$K6_PID" >/dev/null 2>&1 || true
wait "$K6_PID" 2>/dev/null || true

echo "==> Results: cold_start=${COLD:-NaN} scale_delay=${SCALE_DELAY:-NaN} time_to_scale=${TIME_TO_SCALE:-NaN} success=$SUCCESS"

METRICS=$(mktemp)
(
  [ -n "$COLD" ]          && echo "capp_cold_start_seconds $COLD"
  [ -n "$SCALE_DELAY" ]   && echo "capp_scale_delay_seconds $SCALE_DELAY"
  [ -n "$TIME_TO_SCALE" ] && echo "capp_time_to_scale_seconds $TIME_TO_SCALE"
  echo "capp_scale_test_success $SUCCESS"
) > "$METRICS"

IFS=',' read -ra VM_URLS <<< "$VM_IMPORT_URLS"
for url in "${VM_URLS[@]}"; do
  echo "==> Pushing metrics to VictoriaMetrics ($url)..."
  curl -s --data-binary @"$METRICS" \
    "${url%/}/api/v1/import/prometheus?extra_label=capp=${CAPP_NAME}&extra_label=namespace=${CAPP_NAMESPACE}&extra_label=cluster=${CAPP_CLUSTER}"
done
rm -f "$METRICS"

[ "$SUCCESS" -eq 1 ]
