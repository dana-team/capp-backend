package benchmarks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptStubs configures the fake k6, curl and kubectl that a runner script calls.
type scriptStubs struct {
	// K6Exit is the exit code of the fake k6.
	K6Exit int
	// K6Summary is written to the --summary-export path when non-empty.
	K6Summary string
	// PodLines gives the Ready-pod count for each successive kubectl call with jsonpath.
	// The last value repeats. Calls without jsonpath do not consume a value.
	PodLines []string
	// DenyPods makes kubectl exit 1.
	DenyPods bool
	// K6Sleep is how many seconds the fake k6 runs before it exits, so the
	// script can poll pods while k6 is alive.
	K6Sleep int
}

// scriptResult is what a runner script did, as seen by the stubs.
type scriptResult struct {
	Exit int
	// Body is the request body the script POSTed through curl.
	Body string
	// Curl holds one line per curl call, with its arguments.
	Curl []string
}

const fakeK6 = `#!/usr/bin/env bash
sleep "${STUB_K6_SLEEP:-0}"
export_path=""
next=0
for arg in "$@"; do
  if [ "$next" = 1 ]; then export_path="$arg"; next=0; continue; fi
  case "$arg" in
    --summary-export=*) export_path="${arg#--summary-export=}" ;;
    --summary-export) next=1 ;;
  esac
done
if [ -n "$export_path" ] && [ -s "$STUB_DIR/k6-summary.json" ]; then
  cp "$STUB_DIR/k6-summary.json" "$export_path"
fi
exit "$STUB_K6_EXIT"
`

const fakeCurl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_DIR/curl.log"
case "$*" in *"-w "*) echo 0.5 ;; esac
for arg in "$@"; do
  case "$arg" in
    @*) cat "${arg#@}" >> "$STUB_DIR/body" ;;
  esac
done
`

const fakeKubectl = `#!/usr/bin/env bash
if [ "$STUB_DENY_PODS" = 1 ]; then
  echo "forbidden" >&2
  exit 1
fi
has_jsonpath=0
for arg in "$@"; do
  case "$arg" in *jsonpath*) has_jsonpath=1 ;; esac
done
[ "$has_jsonpath" = 1 ] || exit 0

mapfile -t lines < "$STUB_DIR/pod-lines"
n=0
if [ -f "$STUB_DIR/kubectl-calls" ]; then n=$(cat "$STUB_DIR/kubectl-calls"); fi
echo $((n + 1)) > "$STUB_DIR/kubectl-calls"

count=0
if [ "${#lines[@]}" -gt 0 ]; then
  idx=$n
  if [ "$idx" -ge "${#lines[@]}" ]; then idx=$((${#lines[@]} - 1)); fi
  count=${lines[$idx]}
fi
for ((i = 0; i < count; i++)); do echo True; done
`

// runScript runs a script from deploy/benchmark-runner/scripts with the stubs first on PATH.
// name is the script followed by its arguments, such as "run-k6.sh latency".
func runScript(t *testing.T, name string, env map[string]string, stubs scriptStubs) scriptResult {
	t.Helper()
	fields := strings.Fields(name)
	require.NotEmpty(t, fields)

	scriptsDir, err := filepath.Abs("../../../../deploy/benchmark-runner/scripts")
	require.NoError(t, err)

	bin := t.TempDir()
	state := t.TempDir()
	for file, body := range map[string]string{"k6": fakeK6, "curl": fakeCurl, "kubectl": fakeKubectl} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, file), []byte(body), 0o755))
	}
	if stubs.K6Summary != "" {
		require.NoError(t, os.WriteFile(filepath.Join(state, "k6-summary.json"), []byte(stubs.K6Summary), 0o644))
	}
	var pods strings.Builder
	for _, line := range stubs.PodLines {
		pods.WriteString(line + "\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(state, "pod-lines"), []byte(pods.String()), 0o644))

	cmd := exec.Command("bash", append([]string{filepath.Join(scriptsDir, fields[0])}, fields[1:]...)...)
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR=" + state,
		"SCRIPTS_DIR=" + scriptsDir,
		"STUB_DIR=" + state,
		"STUB_K6_EXIT=" + strconv.Itoa(stubs.K6Exit),
		"STUB_K6_SLEEP=" + strconv.Itoa(stubs.K6Sleep),
		"STUB_DENY_PODS=" + map[bool]string{true: "1", false: "0"}[stubs.DenyPods],
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	exit := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit = exitErr.ExitCode()
	} else if err != nil {
		require.NoError(t, err)
	}
	if exit != 0 {
		t.Logf("script output:\n%s", out)
	}

	result := scriptResult{Exit: exit}
	if body, err := os.ReadFile(filepath.Join(state, "body")); err == nil {
		result.Body = string(body)
	}
	if log, err := os.ReadFile(filepath.Join(state, "curl.log")); err == nil {
		result.Curl = strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	}
	return result
}

// baseEnv is the environment the backend gives a latency or throughput Job.
func baseEnv() map[string]string {
	return map[string]string{
		"RUN_ID":         "R1",
		"CAPP_NAME":      "c",
		"CAPP_NAMESPACE": "ns",
		"CAPP_CLUSTER":   "east",
		"CAPP_REVISION":  "c-00007",
		"TARGET_URL":     "http://c.ns.svc.cluster.local",
		"TARGET_PATH":    "/api",
		"VM_IMPORT_URL":  "http://vm-a:8428",
	}
}

// k6Summary is the subset of k6's --summary-export JSON that the scripts read.
// Durations are in milliseconds, as k6 reports them.
func k6Summary(p50, p95, p99, failed, rps float64) string {
	return fmt.Sprintf(
		`{"metrics":{"http_req_duration":{"p(50)":%g,"p(95)":%g,"p(99)":%g},"http_req_failed":{"value":%g},"http_reqs":{"count":1000,"rate":%g}}}`,
		p50, p95, p99, failed, rps)
}

func TestRunK6_PassedRunWritesSummary(t *testing.T) {
	r := runScript(t, "run-k6.sh latency", baseEnv(), scriptStubs{K6Summary: k6Summary(84, 390, 612, 0.002, 30)})
	assert.Equal(t, 0, r.Exit)
	assert.Contains(t, r.Body, `run_id="R1"`)
	assert.Contains(t, r.Body, `result="passed"`)
	assert.Contains(t, r.Body, `reason=""`)
	assert.Contains(t, r.Body, `stat="p95_seconds"} 0.39`)
	assert.Contains(t, r.Body, `stat="start_seconds"}`)
	assert.Contains(t, r.Curl[len(r.Curl)-1], "http://vm-a:8428/api/v1/import/prometheus")
}

func TestRunK6_TooManyErrorsFails(t *testing.T) {
	r := runScript(t, "run-k6.sh latency", baseEnv(), scriptStubs{K6Exit: 99, K6Summary: k6Summary(84, 390, 612, 0.062, 30)})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `reason="6.2% of requests failed, the limit is 1%"`)
}

func TestRunK6_ThroughputReportsRateAndFivePercentLimit(t *testing.T) {
	r := runScript(t, "run-k6.sh throughput", baseEnv(), scriptStubs{K6Exit: 99, K6Summary: k6Summary(1, 2, 3, 0.08, 412.5)})
	assert.Contains(t, r.Body, `reason="8.0% of requests failed, the limit is 5%"`)
	assert.Contains(t, r.Body, `stat="requests_per_second"} 412.5`)
}

func TestRunK6_NoSummaryFileIsRecordedAsBroken(t *testing.T) {
	r := runScript(t, "run-k6.sh latency", baseEnv(), scriptStubs{K6Exit: 107})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `result="failed"`)
	assert.Contains(t, r.Body, `reason="the benchmark could not complete (k6 exited with 107)"`)
}

func TestRunK6_StoresPathWithoutQueryString(t *testing.T) {
	env := baseEnv()
	env["TARGET_PATH"] = "/api/x?token=s3cret"
	r := runScript(t, "run-k6.sh latency", env, scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 1)})
	assert.Contains(t, r.Body, `path="/api/x"`)
	assert.NotContains(t, r.Body, "s3cret")
}

func TestRunK6_EscapesLabelValues(t *testing.T) {
	env := baseEnv()
	env["TARGET_PATH"] = `/a"b\c`
	r := runScript(t, "run-k6.sh latency", env, scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 1)})
	assert.Contains(t, r.Body, `path="/a\"b\\c"`)
}

func TestRunK6_NeverWritesHeaders(t *testing.T) {
	env := baseEnv()
	env["TARGET_HEADERS"] = `{"Authorization":"Bearer s3cret"}`
	r := runScript(t, "run-k6.sh latency", env, scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 1)})
	assert.NotContains(t, r.Body, "s3cret")
}

func TestRunK6_SummaryMissingValuesIsRecordedAsBroken(t *testing.T) {
	summary := `{"metrics":{"http_req_duration":{"p(50)":84,"p(95)":390,"p(99)":612},"http_reqs":{"rate":30}}}`
	r := runScript(t, "run-k6.sh latency", baseEnv(), scriptStubs{K6Summary: summary})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `result="failed"`)
	assert.Contains(t, r.Body, `reason="the benchmark could not complete (k6 summary was missing expected values)"`)
	assert.NotContains(t, r.Body, `stat="p95_seconds"`)
}

// A legitimate zero error ratio is a pass, not missing data.
func TestRunK6_ZeroErrorRatioPasses(t *testing.T) {
	r := runScript(t, "run-k6.sh latency", baseEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 30)})
	assert.Equal(t, 0, r.Exit)
	assert.Contains(t, r.Body, `result="passed"`)
	assert.Contains(t, r.Body, `stat="error_ratio"} 0`)
}

// scaleEnv is the environment the backend gives a scale Job, with the poll
// interval and zero wait set to 0 so the tests run quickly.
func scaleEnv() map[string]string {
	env := baseEnv()
	env["LOAD_VUS"] = "100"
	env["LOAD_DURATION"] = "150s"
	env["BENCHMARK_SA_TOKEN"] = "token"
	env["SCALE_POLL_SECONDS"] = "0"
	env["ZERO_WAIT_SECONDS"] = "0"
	return env
}

// filterContains returns the lines that contain sub.
func filterContains(lines []string, sub string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

func TestScale_ReportsMostPodsWithoutFailing(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 5), PodLines: []string{"0", "0", "1", "3", "3"}, K6Sleep: 1})
	assert.Equal(t, 0, r.Exit)
	assert.Contains(t, r.Body, `result="passed"`)
	assert.Contains(t, r.Body, `stat="max_pods"} 3`)
	assert.Contains(t, r.Body, `stat="time_to_max_pods_seconds"}`)
	assert.Contains(t, r.Body, `stat="scale_out_seconds"}`)
	assert.Contains(t, r.Body, `stat="cold_start_seconds"}`)
}

func TestScale_SinglePodHasNoScaleOutTime(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 5), PodLines: []string{"1"}, K6Sleep: 1})
	assert.Equal(t, 0, r.Exit)
	assert.NotContains(t, r.Body, `stat="scale_out_seconds"`)
	assert.NotContains(t, r.Body, `stat="cold_start_seconds"`, "not at zero pods")
}

func TestScale_NoRequestSucceededIsBroken(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 1.0, 5), PodLines: []string{"0"}, K6Sleep: 1})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `reason="no request succeeded"`)
}

func TestScale_ZeroRequestCountIsBroken(t *testing.T) {
	summary := `{"metrics":{"http_req_failed":{"value":0},"http_reqs":{"count":0,"rate":0}}}`
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: summary, PodLines: []string{"0"}, K6Sleep: 1})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `reason="no request succeeded"`)
}

// A summary without the required request count cannot show that a request succeeded.
func TestScale_MissingRequestCountIsBroken(t *testing.T) {
	summary := `{"metrics":{"http_req_failed":{"value":0},"http_reqs":{"rate":5}}}`
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: summary, PodLines: []string{"0"}, K6Sleep: 1})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `reason="no request succeeded"`)
}

func TestScale_CannotReadPodsIsRecorded(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{DenyPods: true})
	assert.NotEqual(t, 0, r.Exit)
	assert.Contains(t, r.Body, `reason="could not read pods in the namespace"`)
}

func TestScale_ImportsOnlyToFirstURL(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 5), PodLines: []string{"0"}, K6Sleep: 1})
	imports := filterContains(r.Curl, "/api/v1/import/prometheus")
	assert.Len(t, imports, 1)
}

// k6 tags every request with the url and name system tags, and both carry the
// full request URL, including a token in its query string. The k6 scripts must
// limit system tags to the shared list in common.js, which leaves them out.
func TestK6Scripts_SystemTagsExcludeURLAndName(t *testing.T) {
	scriptsDir, err := filepath.Abs("../../../../deploy/benchmark-runner/scripts")
	require.NoError(t, err)

	common, err := os.ReadFile(filepath.Join(scriptsDir, "common.js"))
	require.NoError(t, err)
	listMatch := regexp.MustCompile(`export const systemTags = \[([^\]]*)\];`).FindStringSubmatch(string(common))
	require.NotNil(t, listMatch, "common.js must export systemTags as an array literal")
	tagNames := regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(listMatch[1], -1)
	require.NotEmpty(t, tagNames, "systemTags must list at least one tag")
	for _, m := range tagNames {
		assert.NotEqual(t, "url", m[1], "systemTags must not include url")
		assert.NotEqual(t, "name", m[1], "systemTags must not include name")
	}

	importFromCommon := regexp.MustCompile(`import \{[^}]*\bsystemTags\b[^}]*\} from '\./common\.js';`)
	for _, name := range []string{"latency.js", "throughput.js", "load.js"} {
		src, err := os.ReadFile(filepath.Join(scriptsDir, name))
		require.NoError(t, err)
		text := string(src)
		assert.Regexp(t, importFromCommon, text, "%s must import systemTags from common.js", name)

		optionsAt := strings.Index(text, "export const options")
		defaultAt := strings.Index(text, "export default")
		require.GreaterOrEqual(t, optionsAt, 0, "%s must export options", name)
		require.Greater(t, defaultAt, optionsAt, "%s must export default after options", name)
		assert.Regexp(t, `\bsystemTags\b`, text[optionsAt:defaultAt], "%s must set systemTags in its options", name)
	}
}

// A scale run that never reached a pod has no time to max pods and no scale-out
// time. Only max_pods (0) is recorded for it.
func TestScale_NoReadyPodsOmitsTimeToMax(t *testing.T) {
	r := runScript(t, "scale-test.sh", scaleEnv(), scriptStubs{K6Summary: k6Summary(1, 2, 3, 0, 5), PodLines: []string{"0"}, K6Sleep: 1})
	assert.Contains(t, r.Body, `stat="max_pods"} 0`)
	assert.NotContains(t, r.Body, `stat="time_to_max_pods_seconds"`)
	assert.NotContains(t, r.Body, `stat="scale_out_seconds"`)
}
