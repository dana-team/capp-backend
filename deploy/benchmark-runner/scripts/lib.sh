#!/usr/bin/env bash
# lib.sh holds helpers shared by the benchmark scripts. Source it; do not run it.
#
# Each script records one result line per number, in Prometheus text format, and
# imports them to VictoriaMetrics in a single POST. The labels identify the run.

# label_escape escapes a value for use inside a quoted Prometheus label.
label_escape() {
  printf '%s' "$1" | jq -Rsj 'gsub("\\\\"; "\\\\") | gsub("\""; "\\\"") | gsub("\n"; "\\n")'
}

# write_summary <type> <result> <reason> <stat=value>...
# Reads RUN_ID, CAPP_NAME, CAPP_NAMESPACE, CAPP_CLUSTER, CAPP_REVISION,
# TARGET_PATH and VM_IMPORT_URL. The query string is dropped from the path, so
# request headers and secrets in the query never reach the metrics. Returns non-zero if the import fails.
write_summary() {
  local type="$1" result="$2" reason="$3"
  shift 3

  local path="${TARGET_PATH%%\?*}"
  local labels
  labels="run_id=\"$(label_escape "$RUN_ID")\""
  labels+=",capp=\"$(label_escape "$CAPP_NAME")\""
  labels+=",namespace=\"$(label_escape "$CAPP_NAMESPACE")\""
  labels+=",cluster=\"$(label_escape "$CAPP_CLUSTER")\""
  labels+=",type=\"$(label_escape "$type")\""
  labels+=",path=\"$(label_escape "$path")\""
  labels+=",revision=\"$(label_escape "$CAPP_REVISION")\""
  labels+=",result=\"$(label_escape "$result")\""
  labels+=",reason=\"$(label_escape "$reason")\""

  local body stat
  body=$(mktemp)
  for kv in "$@"; do
    stat="${kv%%=*}"
    printf 'capp_benchmark_result{%s,stat="%s"} %s\n' "$labels" "$stat" "${kv#*=}" >> "$body"
  done

  local status=0
  curl -sS -f --data-binary @"$body" "${VM_IMPORT_URL%/}/api/v1/import/prometheus" >/dev/null || status=$?
  rm -f "$body"
  return "$status"
}
