package capps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const logsPath = "/namespaces/ns1/capps/web/logs"

type followCall struct {
	ctx       context.Context
	pod       string
	container string
	tailLines int64
	sinceTime *time.Time
	log       *io.PipeWriter
}

// fakeStreamer hands every watch and log it opens to the test.
type fakeStreamer struct {
	watches   chan *watch.RaceFreeFakeWatcher
	follows   chan followCall
	watchErr  error
	followErr map[string]error
	// followErrs holds errors for the next opens of a pod's log, one each.
	followErrs map[string][]error
	mu         sync.Mutex
	selectors  chan string
	// rewatchErr fails every watch after the first.
	rewatchErr error
	watched    int
}

func newFakeStreamer() *fakeStreamer {
	return &fakeStreamer{
		watches:    make(chan *watch.RaceFreeFakeWatcher, 10),
		follows:    make(chan followCall, 100),
		selectors:  make(chan string, 10),
		followErr:  map[string]error{},
		followErrs: map[string][]error{},
	}
}

func (f *fakeStreamer) WatchPods(_ context.Context, namespace, selector string) (watch.Interface, error) {
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	f.mu.Lock()
	f.watched++
	again, rewatchErr := f.watched > 1, f.rewatchErr
	f.mu.Unlock()
	if again && rewatchErr != nil {
		return nil, rewatchErr
	}
	f.selectors <- namespace + "/" + selector
	w := watch.NewRaceFreeFake()
	f.watches <- w
	return w, nil
}

func (f *fakeStreamer) FollowLog(ctx context.Context, _, pod, container string, start cluster.LogStart) (io.ReadCloser, error) {
	f.mu.Lock()
	err := f.followErr[pod]
	if errs := f.followErrs[pod]; len(errs) > 0 {
		err, f.followErrs[pod] = errs[0], errs[1:]
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, w := io.Pipe()
	call := followCall{ctx: ctx, pod: pod, container: container, sinceTime: start.SinceTime, log: w}
	if start.TailLines != nil {
		call.tailLines = *start.TailLines
	}
	f.follows <- call
	return r, nil
}

func (f *fakeStreamer) nextWatch(t *testing.T) *watch.RaceFreeFakeWatcher {
	t.Helper()
	select {
	case w := <-f.watches:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("no pod watch was opened")
		return nil
	}
}

func (f *fakeStreamer) nextFollow(t *testing.T) followCall {
	t.Helper()
	select {
	case call := <-f.follows:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("no log was opened")
		return followCall{}
	}
}

func (f *fakeStreamer) noFollow(t *testing.T) {
	t.Helper()
	select {
	case call := <-f.follows:
		t.Fatalf("log of pod %s was opened", call.pod)
	case <-time.After(100 * time.Millisecond):
	}
}

func logPod(name, container string, started bool, restarts int32) *corev1.Pod {
	state := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	if started {
		state = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", Labels: map[string]string{parentCappLabel: "web"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container}, {Name: "queue-proxy"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: container, State: state, RestartCount: restarts},
			{Name: "queue-proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
}

func appCapp() *cappv1alpha1.Capp {
	capp := makeCapp("web", "ns1")
	capp.Spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{{Name: "app"}}
	return capp
}

func logEngine(t *testing.T, streamer cluster.PodStream, objects ...client.Object) *gin.Engine {
	t.Helper()
	return logEngineWith(t, mustHandler(t, false, nil, nil, makeSizes()), streamer, objects...)
}

func logEngineWith(t *testing.T, h *Handler, streamer cluster.PodStream, objects ...client.Object) *gin.Engine {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	_, engine := gin.CreateTestContext(httptest.NewRecorder())
	engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		if streamer != nil {
			c.Set(string(middleware.PodStreamKey), streamer)
		}
		c.Next()
	})
	h.RegisterRoutes(engine.Group(""))
	return engine
}

type sseEvent struct {
	name string
	data string
}

type sseClient struct {
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

func openStream(t *testing.T, engine *gin.Engine, query string) *sseClient {
	t.Helper()
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+logsPath+query, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	events := make(chan sseEvent, 100)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if ev.name != "" {
					events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return &sseClient{resp: resp, events: events, cancel: cancel}
}

func (s *sseClient) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		require.True(t, ok, "the stream ended")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event came")
		return sseEvent{}
	}
}

func (s *sseClient) closed(t *testing.T) {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		require.False(t, ok, "unexpected event %v", ev)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
}

func decodeEvent[T any](t *testing.T, ev sseEvent, name string) T {
	t.Helper()
	require.Equal(t, name, ev.name, ev.data)
	var v T
	require.NoError(t, json.Unmarshal([]byte(ev.data), &v))
	return v
}

func writeLine(t *testing.T, call followCall, line string) {
	t.Helper()
	_, err := io.WriteString(call.log, line+"\n")
	require.NoError(t, err)
}

func TestLogsRefusedBeforeTheStream(t *testing.T) {
	forbidden := k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no"))
	for name, tc := range map[string]struct {
		streamer cluster.PodStream
		query    string
		objects  []client.Object
		status   int
	}{
		"no pod stream in this mode": {streamer: nil, objects: []client.Object{appCapp()}, status: http.StatusNotImplemented},
		"tailLines too large":        {streamer: newFakeStreamer(), query: "?tailLines=1001", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"tailLines negative":         {streamer: newFakeStreamer(), query: "?tailLines=-1", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"bad container":              {streamer: newFakeStreamer(), query: "?container=Bad_Name", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"bad pod":                    {streamer: newFakeStreamer(), query: "?pod=Bad_Name", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"bad sinceTime":              {streamer: newFakeStreamer(), query: "?sinceTime=yesterday", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"sinceTime and tailLines":    {streamer: newFakeStreamer(), query: "?sinceTime=2026-11-18T10:00:00Z&tailLines=5", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"podSinceTime and tailLines": {streamer: newFakeStreamer(), query: "?podSinceTime=web-a=2026-11-18T10:00:00Z&tailLines=5", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"podSinceTime without pod":   {streamer: newFakeStreamer(), query: "?podSinceTime=2026-11-18T10:00:00Z", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"podSinceTime bad pod":       {streamer: newFakeStreamer(), query: "?podSinceTime=Bad_Pod=2026-11-18T10:00:00Z", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"podSinceTime bad time":      {streamer: newFakeStreamer(), query: "?podSinceTime=web-a=yesterday", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"podSinceTime pod twice":     {streamer: newFakeStreamer(), query: "?podSinceTime=web-a=2026-11-18T10:00:00Z&podSinceTime=web-a=2026-11-18T10:00:01Z", objects: []client.Object{appCapp()}, status: http.StatusBadRequest},
		"no such Capp":               {streamer: newFakeStreamer(), status: http.StatusNotFound},
		"pod watch forbidden":        {streamer: &fakeStreamer{watchErr: forbidden}, objects: []client.Object{appCapp()}, status: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			w := testutil.ServeHTTP(logEngine(t, tc.streamer, tc.objects...), http.MethodGet, logsPath+tc.query, nil)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
		})
	}
}

func TestLogsStreamsEveryPodAndJoinsNewOnes(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "?tailLines=5")
	podWatch := streamer.nextWatch(t)
	assert.Equal(t, "ns1/"+parentCappLabel+"=web", <-streamer.selectors)

	podWatch.Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	assert.Equal(t, "web-a", a.pod)
	assert.Equal(t, "app", a.container, "the Capp's container, not queue-proxy")
	assert.Equal(t, int64(5), a.tailLines)
	assert.Equal(t, LogPodEvent{Pod: "web-a"}, decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted))

	writeLine(t, a, "2026-11-18T10:00:00.123456789Z hello world")
	assert.Equal(t, LogLine{Pod: "web-a", Container: "app", Timestamp: "2026-11-18T10:00:00.123456789Z", Line: "hello world"},
		decodeEvent[LogLine](t, stream.next(t), LogEventLine))

	// A pod that starts later joins once its container runs.
	podWatch.Add(logPod("web-b", "app", false, 0))
	streamer.noFollow(t)
	podWatch.Modify(logPod("web-b", "app", true, 0))
	b := streamer.nextFollow(t)
	assert.Equal(t, "web-b", b.pod)
	assert.Equal(t, LogPodEvent{Pod: "web-b"}, decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted))

	writeLine(t, b, "no timestamp here")
	assert.Equal(t, LogLine{Pod: "web-b", Container: "app", Line: "no timestamp here"}, decodeEvent[LogLine](t, stream.next(t), LogEventLine))

	// Further changes of a followed pod open nothing new.
	podWatch.Modify(logPod("web-a", "app", true, 0))
	streamer.noFollow(t)

	podWatch.Delete(logPod("web-a", "app", true, 0))
	assert.Equal(t, LogPodEvent{Pod: "web-a", Reason: "pod deleted"}, decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded))
	assert.Error(t, a.ctx.Err(), "the deleted pod's log request is cancelled")
}

func TestLogsDefaultContainerIsKnativeName(t *testing.T) {
	streamer := newFakeStreamer()
	openStream(t, logEngine(t, streamer, makeCapp("web", "ns1")), "")
	streamer.nextWatch(t).Add(logPod("web-a", knativeDefaultContainer, true, 0))
	call := streamer.nextFollow(t)
	assert.Equal(t, knativeDefaultContainer, call.container)
	assert.Equal(t, int64(defaultLogTailLines), call.tailLines)
}

func TestLogsFollowsARestartedContainerAgain(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)

	podWatch.Add(logPod("web-a", "app", true, 0))
	first := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	require.NoError(t, first.log.Close())
	assert.Equal(t, LogPodEvent{Pod: "web-a", Reason: "container stopped"}, decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded))

	// The same container is not followed twice.
	podWatch.Modify(logPod("web-a", "app", true, 0))
	streamer.noFollow(t)

	podWatch.Modify(logPod("web-a", "app", true, 1))
	second := streamer.nextFollow(t)
	assert.Equal(t, int64(maxLogTailLines), second.tailLines, "a restarted container's log is new, so all of it is sent")
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
}

func TestLogsOnlyThePodAskedFor(t *testing.T) {
	streamer := newFakeStreamer()
	openStream(t, logEngine(t, streamer, appCapp()), "?pod=web-b")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))
	podWatch.Add(logPod("web-b", "app", true, 0))
	assert.Equal(t, "web-b", streamer.nextFollow(t).pod)
	streamer.noFollow(t)
}

func TestLogsNoticeForAPodWithoutTheContainer(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "?container=sidecar")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))
	podWatch.Modify(logPod("web-a", "app", true, 0))
	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventNotice)
	assert.Equal(t, "pod web-a has no container sidecar", notice.Message)
	streamer.noFollow(t)
	select {
	case ev := <-stream.events:
		t.Fatalf("the notice is sent once, got %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLogsCapsThePodsFollowed(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)

	calls := map[string]followCall{}
	for i := range maxLogStreamPods {
		podWatch.Add(logPod(fmt.Sprintf("web-%02d", i), "app", true, 0))
		call := streamer.nextFollow(t)
		calls[call.pod] = call
		decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	}
	podWatch.Add(logPod("web-extra", "app", true, 0))
	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventNotice)
	assert.Contains(t, notice.Message, "web-extra")
	streamer.noFollow(t)

	// When a pod ends, the waiting one joins.
	require.NoError(t, calls["web-00"].log.Close())
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded)
	assert.Equal(t, "web-extra", streamer.nextFollow(t).pod)
}

func TestLogsRewatchesWithoutFollowingAPodTwice(t *testing.T) {
	old := logRewatch
	logRewatch = time.Millisecond
	t.Cleanup(func() { logRewatch = old })

	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)

	podWatch.Stop()
	again := streamer.nextWatch(t)
	again.Add(logPod("web-a", "app", true, 0))
	again.Add(logPod("web-b", "app", true, 0))
	assert.Equal(t, "web-b", streamer.nextFollow(t).pod)
	streamer.noFollow(t)

	writeLine(t, a, "still here")
	for {
		ev := stream.next(t)
		if ev.name == LogEventLine {
			assert.Equal(t, "still here", decodeEvent[LogLine](t, ev, LogEventLine).Line)
			break
		}
	}
}

func TestLogsWatchErrorEndsTheStream(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)
	podWatch.Error(&metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden, Reason: metav1.StatusReasonForbidden, Message: "no watch"})
	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventError)
	assert.Contains(t, notice.Message, "no watch")
	stream.closed(t)
}

func TestLogsRefusedLogEndsTheStream(t *testing.T) {
	streamer := newFakeStreamer()
	streamer.followErr["web-a"] = k8serrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-a", errors.New("no"))
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventError)
	assert.Contains(t, notice.Message, "web-a")
	stream.closed(t)
}

func TestLogsEndsAfterTheMaximumDuration(t *testing.T) {
	old := logStreamDuration
	logStreamDuration = 200 * time.Millisecond
	t.Cleanup(func() { logStreamDuration = old })

	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)

	decodeEvent[LogNotice](t, stream.next(t), LogEventEnd)
	stream.closed(t)
	assert.Error(t, a.ctx.Err(), "the pod's log request ends with the stream")
}

func TestLogsClientGoneStopsEverything(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)

	stream.cancel()
	assert.Eventually(t, func() bool { return a.ctx.Err() != nil }, 5*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool {
		_, err := io.WriteString(a.log, "nobody reads this\n")
		return errors.Is(err, io.ErrClosedPipe)
	}, 5*time.Second, 10*time.Millisecond, "the log stream is closed")
}

func shortRetries(t *testing.T) {
	t.Helper()
	old := logRetryBase
	logRetryBase = time.Millisecond
	t.Cleanup(func() { logRetryBase = old })
}

func TestLogsRewatchFailureEndsTheStreamCleanly(t *testing.T) {
	old := logRewatch
	logRewatch = time.Millisecond
	t.Cleanup(func() { logRewatch = old })

	streamer := newFakeStreamer()
	streamer.rewatchErr = errors.New("api server gone")
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)

	podWatch.Stop()
	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventError)
	assert.Contains(t, notice.Message, "api server gone")
	stream.closed(t)
	assert.Error(t, a.ctx.Err(), "the pod's log request ends with the stream")
}

func TestLogsRetriesALogThatFailsToOpen(t *testing.T) {
	shortRetries(t)
	streamer := newFakeStreamer()
	streamer.followErrs["web-a"] = []error{errors.New("kubelet busy")}
	stream := openStream(t, logEngine(t, streamer, appCapp()), "?tailLines=7")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))

	notice := decodeEvent[LogNotice](t, stream.next(t), LogEventNotice)
	assert.Contains(t, notice.Message, "kubelet busy")
	assert.Contains(t, notice.Message, "trying again")
	// No pod event follows; the retry opens the log by itself.
	call := streamer.nextFollow(t)
	assert.Equal(t, "web-a", call.pod)
	assert.Equal(t, int64(7), call.tailLines)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
}

func TestLogsResumesABrokenLogAfterTheLastLine(t *testing.T) {
	shortRetries(t)
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	first := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	writeLine(t, first, "2026-11-18T10:00:00.5Z one")
	decodeEvent[LogLine](t, stream.next(t), LogEventLine)

	require.NoError(t, first.log.CloseWithError(errors.New("connection reset")))
	assert.Equal(t, LogPodEvent{Pod: "web-a", Reason: "log stream failed"}, decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded))
	assert.Contains(t, decodeEvent[LogNotice](t, stream.next(t), LogEventNotice).Message, "trying again")

	second := streamer.nextFollow(t)
	require.NotNil(t, second.sinceTime, "the retry goes on from the last line")
	assert.Equal(t, "2026-11-18T10:00:00.5Z", second.sinceTime.Format(time.RFC3339Nano))
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	// The API server starts on a whole second, so lines sent already come
	// again and are left out.
	writeLine(t, second, "2026-11-18T10:00:00.1Z before")
	writeLine(t, second, "2026-11-18T10:00:00.5Z one")
	writeLine(t, second, "2026-11-18T10:00:01Z two")
	assert.Equal(t, "two", decodeEvent[LogLine](t, stream.next(t), LogEventLine).Line)
}

func TestLogsGivesUpAfterRepeatedFailures(t *testing.T) {
	shortRetries(t)
	streamer := newFakeStreamer()
	for range maxLogRetries + 1 {
		streamer.followErrs["web-a"] = append(streamer.followErrs["web-a"], errors.New("kubelet busy"))
	}
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))

	for range maxLogRetries {
		assert.Contains(t, decodeEvent[LogNotice](t, stream.next(t), LogEventNotice).Message, "trying again")
	}
	assert.Contains(t, decodeEvent[LogNotice](t, stream.next(t), LogEventNotice).Message, "times in a row")
	streamer.noFollow(t)

	// The same container is not tried again; a restarted one is.
	podWatch.Modify(logPod("web-a", "app", true, 0))
	streamer.noFollow(t)
	podWatch.Modify(logPod("web-a", "app", true, 1))
	assert.Equal(t, "web-a", streamer.nextFollow(t).pod)
}

func TestLogsSinceTime(t *testing.T) {
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "?sinceTime=2026-11-18T10:00:00.5Z")
	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	call := streamer.nextFollow(t)
	require.NotNil(t, call.sinceTime)
	assert.Equal(t, int64(0), call.tailLines, "sinceTime replaces tailLines")
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	writeLine(t, call, "2026-11-18T10:00:00.5Z the client has this one")
	writeLine(t, call, "2026-11-18T10:00:00.6Z new")
	assert.Equal(t, "new", decodeEvent[LogLine](t, stream.next(t), LogEventLine).Line)
}

func TestLogsPodSinceTime(t *testing.T) {
	streamer := newFakeStreamer()
	// web-a's newer line came before web-b's older one, so each pod
	// resumes from its own last line; web-c, not named, from sinceTime.
	stream := openStream(t, logEngine(t, streamer, appCapp()),
		"?podSinceTime=web-a=2026-11-18T10:00:10Z&podSinceTime=web-b=2026-11-18T10:00:08Z&sinceTime=2026-11-18T10:00:08Z")
	podWatch := streamer.nextWatch(t)

	since := map[string]string{}
	calls := map[string]followCall{}
	for _, name := range []string{"web-a", "web-b", "web-c"} {
		podWatch.Add(logPod(name, "app", true, 0))
		call := streamer.nextFollow(t)
		require.NotNil(t, call.sinceTime, name)
		since[call.pod] = call.sinceTime.Format(time.RFC3339)
		calls[call.pod] = call
		decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	}
	assert.Equal(t, map[string]string{
		"web-a": "2026-11-18T10:00:10Z",
		"web-b": "2026-11-18T10:00:08Z",
		"web-c": "2026-11-18T10:00:08Z",
	}, since)

	writeLine(t, calls["web-b"], "2026-11-18T10:00:08Z the client has this one")
	writeLine(t, calls["web-b"], "2026-11-18T10:00:09Z unseen, older than web-a's last line")
	assert.Equal(t, "unseen, older than web-a's last line", decodeEvent[LogLine](t, stream.next(t), LogEventLine).Line)
}

func TestLogsRetryAfterARestartResumes(t *testing.T) {
	shortRetries(t)
	streamer := newFakeStreamer()
	stream := openStream(t, logEngine(t, streamer, appCapp()), "")
	podWatch := streamer.nextWatch(t)
	podWatch.Add(logPod("web-a", "app", true, 0))
	first := streamer.nextFollow(t)
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	require.NoError(t, first.log.Close())
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded)

	restarted := logPod("web-a", "app", true, 1)
	startedAt := time.Date(2026, 11, 18, 10, 5, 0, 0, time.UTC)
	restarted.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(startedAt)
	podWatch.Modify(restarted)
	second := streamer.nextFollow(t)
	require.NotNil(t, second.sinceTime, "a restarted container is read from its start")
	assert.False(t, second.sinceTime.After(startedAt))
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodStarted)
	writeLine(t, second, "2026-11-18T10:05:01Z new container")
	assert.Equal(t, "new container", decodeEvent[LogLine](t, stream.next(t), LogEventLine).Line)

	// The retry goes on from the last line, not from the restart again.
	require.NoError(t, second.log.CloseWithError(errors.New("connection reset")))
	decodeEvent[LogPodEvent](t, stream.next(t), LogEventPodEnded)
	decodeEvent[LogNotice](t, stream.next(t), LogEventNotice)
	third := streamer.nextFollow(t)
	require.NotNil(t, third.sinceTime)
	assert.Equal(t, "2026-11-18T10:05:01Z", third.sinceTime.Format(time.RFC3339))
	assert.Equal(t, int64(0), third.tailLines)
}

func TestLogsCapsTheOpenStreams(t *testing.T) {
	streamer := newFakeStreamer()
	engine := logEngineWith(t, mustHandler(t, false, nil, nil, makeSizes()).WithMaxLogStreams(1), streamer, appCapp())
	first := openStream(t, engine, "")
	streamer.nextWatch(t)

	w := testutil.ServeHTTP(engine, http.MethodGet, logsPath, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "LOG_STREAMS_BUSY")

	first.cancel()
	assert.Eventually(t, func() bool {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server := httptest.NewServer(engine)
		defer server.Close()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+logsPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "a closed stream frees its slot")
}

func TestLogsClientThatStopsReadingEndsTheStream(t *testing.T) {
	old := logWriteTimeout
	logWriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { logWriteTimeout = old })

	streamer := newFakeStreamer()
	server := httptest.NewServer(logEngine(t, streamer, appCapp()))
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: test\r\n\r\n", logsPath)
	require.NoError(t, err)

	streamer.nextWatch(t).Add(logPod("web-a", "app", true, 0))
	a := streamer.nextFollow(t)
	// Nobody reads conn: once the socket buffers are full a write blocks
	// and its deadline ends the stream.
	line := strings.Repeat("x", 32*1024) + "\n"
	go func() {
		for a.ctx.Err() == nil {
			if _, err := io.WriteString(a.log, line); err != nil {
				return
			}
		}
	}()
	assert.Eventually(t, func() bool { return a.ctx.Err() != nil }, 10*time.Second, 20*time.Millisecond)
}

func TestLogsHeartbeat(t *testing.T) {
	old := logHeartbeat
	logHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { logHeartbeat = old })

	streamer := newFakeStreamer()
	engine := logEngine(t, streamer, appCapp())
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	resp, err := http.Get(server.URL + logsPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, ": keepalive\n", line)
}

func TestReadLogLines(t *testing.T) {
	long := strings.Repeat("x", maxLogLineBytes+100)
	var lines []string
	err := readLogLines(strings.NewReader("one\r\ntwo\n"+long+"\nlast"), func(line string) bool {
		lines = append(lines, line)
		return true
	})
	require.NoError(t, err)
	require.Len(t, lines, 4)
	assert.Equal(t, []string{"one", "two"}, lines[:2])
	assert.Len(t, lines[2], maxLogLineBytes, "a long line is cut")
	assert.Equal(t, "last", lines[3], "a last line without a newline is kept")
}

func TestSplitTimestamp(t *testing.T) {
	for raw, want := range map[string][2]string{
		"2026-11-18T10:00:00Z a b": {"2026-11-18T10:00:00Z", "a b"},
		"2026-11-18T10:00:00Z":     {"2026-11-18T10:00:00Z", ""},
		"not a time a b":           {"", "not a time a b"},
		"":                         {"", ""},
	} {
		ts, line := splitTimestamp(raw)
		assert.Equal(t, want, [2]string{ts, line}, raw)
	}
}
