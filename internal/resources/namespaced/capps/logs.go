package capps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// parentCappLabel is the label the operator puts on the pods of a Capp
	// (through the Knative Service's template).
	parentCappLabel = "rcs.dana.io/parent-capp"
	// knativeDefaultContainer is the name Knative gives a container the
	// Capp leaves unnamed.
	knativeDefaultContainer = "user-container"

	defaultLogTailLines = 100
	maxLogTailLines     = 1000
	// maxLogStreamPods caps the logs one stream follows at a time; a pod
	// over the cap is announced with a notice and joins when one ends.
	maxLogStreamPods = 20
	// maxLogLineBytes caps one line; the rest of a longer line is dropped.
	maxLogLineBytes = 64 * 1024

	// maxLogStreamDuration ends every stream after this long, with an end
	// event, so that connections nobody reads any more do not stay open
	// for good. The client connects again.
	maxLogStreamDuration = 30 * time.Minute
	logHeartbeatInterval = 15 * time.Second
	logRewatchDelay      = time.Second
	// logWriteTimeoutDuration bounds one write to the client, so a client
	// that stops reading ends its stream instead of holding it open.
	logWriteTimeoutDuration = 30 * time.Second
	// A pod's log that fails to open or breaks off is tried again after
	// logRetryBaseDelay, doubling up to logRetryMaxDelay, maxLogRetries
	// times in a row before the stream gives up on that container.
	logRetryBaseDelay = time.Second
	logRetryMaxDelay  = 30 * time.Second
	maxLogRetries     = 5
	// maxPodSinceTimes caps the podSinceTime entries of one request.
	maxPodSinceTimes = 100

	// defaultMaxLogStreams is resources.capps.maxLogStreams's default.
	defaultMaxLogStreams = 100
)

// Variables, not constants, so that tests can make them short.
var (
	logStreamDuration = maxLogStreamDuration
	logHeartbeat      = logHeartbeatInterval
	logRewatch        = logRewatchDelay
	logWriteTimeout   = logWriteTimeoutDuration
	logRetryBase      = logRetryBaseDelay
)

// logs handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/logs
//
// It answers a text/event-stream that follows the log of every pod of the
// Capp, joining pods as they start, until the client goes away or
// maxLogStreamDuration passes. Query: pod (only this pod), container
// (default: the Capp's container), tailLines (lines of history per pod,
// default 100) or, to resume a stream, podSinceTime (repeated
// <pod>=<time>: that pod's lines after the time) and sinceTime (the same for
// every pod not named).
// Errors found before the stream starts are ordinary JSON error responses;
// later ones are error events.
func (h *Handler) logs(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	streamer, ok := podStream(c)
	if !ok {
		apierrors.Respond(c, apierrors.NewNotSupported("Capp logs in this authorization mode"))
		return
	}

	opts, err := parseLogOptions(c)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	select {
	case h.logStreams <- struct{}{}:
		defer func() { <-h.logStreams }()
	default:
		apierrors.Respond(c, apierrors.NewLogStreamsBusy())
		return
	}

	namespace := c.Param("namespace")
	name := c.Param("name")
	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}
	if opts.container == "" {
		opts.container = cappContainerName(&capp)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), logStreamDuration)
	defer cancel()

	selector := parentCappLabel + "=" + name
	// The watch opens before the response starts, so a caller who may not
	// watch pods gets a 403, not a stream that ends at once.
	podWatch, err := streamer.WatchPods(ctx, namespace, selector)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	// The server's write timeout would close the stream after 30 seconds,
	// so every write sets its own deadline instead.
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetWriteDeadline(time.Now().Add(logWriteTimeout))
	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	// nginx (OpenShift routes and most ingresses) buffers a response
	// unless told not to.
	header.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	if err := rc.Flush(); err != nil {
		podWatch.Stop()
		return
	}

	s := &logSession{
		ctx:       ctx,
		cancel:    cancel,
		w:         c.Writer,
		rc:        rc,
		streamer:  streamer,
		namespace: namespace,
		selector:  selector,
		opts:      opts,
		msgs:      make(chan podMsg, 256),
		active:    map[string]*followedPod{},
		ended:     map[string]int32{},
		noticed:   map[string]bool{},
		pods:      map[string]*corev1.Pod{},
		// A resumed stream goes on where the client's pods left off, as a
		// retry does.
		lastSeen: opts.podSinceTimes,
		retries:  map[string]int{},
		retrying: map[string]bool{},
	}
	s.run(podWatch)
}

type logOptions struct {
	pod       string
	container string
	tailLines int64
	// sinceTime, when set, replaces tailLines: each pod's lines after it.
	sinceTime *time.Time
	// podSinceTimes holds, per pod, the timestamp of the last line the
	// client has (podSinceTime); it wins over sinceTime for that pod,
	// because the lines of different pods do not arrive in time order.
	podSinceTimes map[string]time.Time
}

func parseLogOptions(c *gin.Context) (logOptions, error) {
	opts := logOptions{pod: c.Query("pod"), container: c.Query("container"), tailLines: defaultLogTailLines}
	if opts.pod != "" && len(validation.IsDNS1123Subdomain(opts.pod)) > 0 {
		return opts, apierrors.NewBadRequest("pod must be a valid pod name")
	}
	if opts.container != "" && len(validation.IsDNS1123Label(opts.container)) > 0 {
		return opts, apierrors.NewBadRequest("container must be a valid container name")
	}
	if raw := c.Query("tailLines"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 || parsed > maxLogTailLines {
			return opts, apierrors.NewBadRequest(fmt.Sprintf("tailLines must be between 0 and %d", maxLogTailLines))
		}
		opts.tailLines = parsed
	}
	if raw := c.Query("sinceTime"); raw != "" {
		if c.Query("tailLines") != "" {
			return opts, apierrors.NewBadRequest("set tailLines or sinceTime, not both")
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return opts, apierrors.NewBadRequest("sinceTime must be an RFC 3339 time")
		}
		opts.sinceTime = &parsed
	}
	podSinceTimes, err := parsePodSinceTimes(c.QueryArray("podSinceTime"))
	if err != nil {
		return opts, err
	}
	if len(podSinceTimes) > 0 && c.Query("tailLines") != "" {
		return opts, apierrors.NewBadRequest("set tailLines or podSinceTime, not both")
	}
	opts.podSinceTimes = podSinceTimes
	return opts, nil
}

// parsePodSinceTimes reads podSinceTime values, each <pod>=<RFC 3339 time>.
func parsePodSinceTimes(values []string) (map[string]time.Time, error) {
	if len(values) > maxPodSinceTimes {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("at most %d podSinceTime values", maxPodSinceTimes))
	}
	times := make(map[string]time.Time, len(values))
	for _, value := range values {
		pod, raw, found := strings.Cut(value, "=")
		if !found || len(validation.IsDNS1123Subdomain(pod)) > 0 {
			return nil, apierrors.NewBadRequest("podSinceTime must be <pod>=<RFC 3339 time> with a valid pod name")
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, apierrors.NewBadRequest("podSinceTime must be <pod>=<RFC 3339 time>; " + pod + " has no valid time")
		}
		if _, dup := times[pod]; dup {
			return nil, apierrors.NewBadRequest("podSinceTime names pod " + pod + " twice")
		}
		times[pod] = parsed
	}
	return times, nil
}

// cappContainerName is the name of the Capp's container in its pods, which
// also run Knative's queue-proxy.
func cappContainerName(capp *cappv1alpha1.Capp) string {
	containers := capp.Spec.ConfigurationSpec.Template.Spec.Containers
	if len(containers) > 0 && containers[0].Name != "" {
		return containers[0].Name
	}
	return knativeDefaultContainer
}

func podStream(c *gin.Context) (cluster.PodStream, bool) {
	value, exists := c.Get(string(middleware.PodStreamKey))
	if !exists {
		return nil, false
	}
	streamer, ok := value.(cluster.PodStream)
	return streamer, ok && streamer != nil
}

// podMsg is what a pod's goroutine tells the session: the log opened, a
// line, or the end (with the error that ended it, nil when the container
// stopped). podRetry comes from a timer: try the pod's log again.
type podMsg struct {
	kind   podMsgKind
	pod    string
	line   LogLine
	err    error
	opened bool
}

type podMsgKind int

const (
	podOpened podMsgKind = iota
	podLine
	podDone
	podRetry
)

type followedPod struct {
	cancel   context.CancelFunc
	restarts int32
	reason   string
}

// logSession is one open stream. Only run's goroutine touches its maps and
// writes to the response; each followed pod has a goroutine that reads the
// log and sends podMsgs.
type logSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	w         gin.ResponseWriter
	rc        *http.ResponseController
	streamer  cluster.PodStream
	namespace string
	selector  string
	opts      logOptions
	msgs      chan podMsg
	wg        sync.WaitGroup

	// active holds the pods whose log is followed now.
	active map[string]*followedPod
	// ended holds, per pod, the restart count of the container whose log
	// was followed to its end, so that the same container is not
	// followed again and a restarted one is.
	ended map[string]int32
	// noticed holds the pods a notice was sent about, so it is sent once.
	noticed map[string]bool
	// pods holds the last state seen of each pod, to start a pod that was
	// over the cap when another one ends.
	pods map[string]*corev1.Pod
	// lastSeen holds, per pod, the timestamp of the last line sent, so a
	// log that broke off goes on after it.
	lastSeen map[string]time.Time
	// retries holds, per pod, the failed attempts in a row.
	retries map[string]int
	// retrying holds the pods that wait for a retry; nothing else starts
	// them before it.
	retrying map[string]bool
}

func (s *logSession) run(podWatch watch.Interface) {
	defer func() {
		podWatch.Stop()
		// Ends every pod's goroutine, also one that waits to send.
		s.cancel()
		s.wg.Wait()
	}()

	heartbeat := time.NewTicker(logHeartbeat)
	defer heartbeat.Stop()

	for {
		var err error
		select {
		case <-s.ctx.Done():
			if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
				_ = s.event(LogEventEnd, LogNotice{Message: "the stream reached its maximum duration; connect again to go on"})
			}
			return
		case ev, open := <-podWatch.ResultChan():
			if open && ev.Type != watch.Error {
				err = s.onPodEvent(ev)
				break
			}
			podWatch.Stop()
			// podWatch keeps the stopped watch when this fails, so the
			// deferred Stop never sees nil.
			next, watchErr := s.watchEnded(ev, open)
			if watchErr != nil {
				if s.ctx.Err() == nil {
					_ = s.event(LogEventError, LogNotice{Message: "watching the Capp's pods failed: " + watchErr.Error()})
				}
				return
			}
			podWatch = next
		case msg := <-s.msgs:
			err = s.onPodMsg(msg)
		case <-heartbeat.C:
			// An SSE comment; it keeps proxies from closing an idle stream.
			err = s.write(": keepalive\n\n", true)
		}
		if err != nil {
			// The client went away.
			return
		}
	}
}

// watchEnded handles a pod watch that closed (open is false) or sent an
// error event. A closed or expired watch is opened again: with no
// resourceVersion the new watch starts with ADDED events for the pods that
// exist, and the ones followed already are skipped. Any other error is
// returned.
func (s *logSession) watchEnded(ev watch.Event, open bool) (watch.Interface, error) {
	if open {
		if status := k8serrors.FromObject(ev.Object); !k8serrors.IsGone(status) && !k8serrors.IsResourceExpired(status) {
			return nil, status
		}
	}
	// The API server ends a watch after a while; a pause keeps a watch
	// that closes at once from spinning.
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-time.After(logRewatch):
	}
	return s.streamer.WatchPods(s.ctx, s.namespace, s.selector)
}

func (s *logSession) onPodEvent(ev watch.Event) error {
	pod, ok := ev.Object.(*corev1.Pod)
	if !ok {
		return nil
	}
	if s.opts.pod != "" && pod.Name != s.opts.pod {
		return nil
	}
	if ev.Type == watch.Deleted {
		delete(s.pods, pod.Name)
		delete(s.retrying, pod.Name)
		if followed, ok := s.active[pod.Name]; ok {
			followed.reason = "pod deleted"
			followed.cancel()
		}
		return nil
	}
	s.pods[pod.Name] = pod
	return s.consider(pod)
}

// consider starts following pod when its container runs (or ran) and is not
// followed yet.
func (s *logSession) consider(pod *corev1.Pod) error {
	if _, ok := s.active[pod.Name]; ok || s.retrying[pod.Name] {
		return nil
	}
	exists, started, restarts, startedAt := containerState(pod, s.opts.container)
	if !exists {
		return s.noticeOnce(pod.Name, fmt.Sprintf("pod %s has no container %s", pod.Name, s.opts.container))
	}
	if !started {
		return nil
	}
	var start cluster.LogStart
	// Lines up to skip were sent already (or are older than sinceTime).
	var skip time.Time
	fresh := false
	if last, seen := s.ended[pod.Name]; seen {
		if restarts <= last {
			return nil
		}
		// A restarted container has a new log; all of it is new to the
		// client. It starts at the container's start, which also becomes
		// the place a retry goes on from until a line comes. The ended
		// entry goes, so that a retry is not taken for another restart.
		delete(s.ended, pod.Name)
		delete(s.lastSeen, pod.Name)
		if startedAt.IsZero() {
			fresh = true
		} else {
			s.lastSeen[pod.Name] = startedAt.Add(-time.Nanosecond)
		}
	}
	if fresh {
		tail := int64(maxLogTailLines)
		start.TailLines = &tail
	} else if last, seen := s.lastSeen[pod.Name]; seen {
		// The log broke off: go on after the last line sent.
		start.SinceTime, skip = &last, last
	} else if s.opts.sinceTime != nil {
		start.SinceTime, skip = s.opts.sinceTime, *s.opts.sinceTime
	} else {
		tail := s.opts.tailLines
		start.TailLines = &tail
	}
	if len(s.active) >= maxLogStreamPods {
		return s.noticeOnce(pod.Name, fmt.Sprintf("pod %s is not followed: the stream follows %d pods already", pod.Name, maxLogStreamPods))
	}
	delete(s.noticed, pod.Name)

	podCtx, cancel := context.WithCancel(s.ctx)
	s.active[pod.Name] = &followedPod{cancel: cancel, restarts: restarts}
	s.wg.Add(1)
	go s.follow(podCtx, pod.Name, start, skip)
	return nil
}

func (s *logSession) noticeOnce(pod, message string) error {
	if s.noticed[pod] {
		return nil
	}
	s.noticed[pod] = true
	return s.event(LogEventNotice, LogNotice{Message: message})
}

func (s *logSession) onPodMsg(msg podMsg) error {
	switch msg.kind {
	case podOpened:
		return s.event(LogEventPodStarted, LogPodEvent{Pod: msg.pod})
	case podLine:
		// A line came, so the log works again.
		delete(s.retries, msg.pod)
		if ts, err := time.Parse(time.RFC3339Nano, msg.line.Timestamp); err == nil {
			s.lastSeen[msg.pod] = ts
		}
		return s.event(LogEventLine, msg.line)
	case podRetry:
		delete(s.retrying, msg.pod)
		if pod, ok := s.pods[msg.pod]; ok {
			return s.consider(pod)
		}
		return nil
	}

	followed := s.active[msg.pod]
	delete(s.active, msg.pod)
	if followed == nil {
		return nil
	}
	followed.cancel()
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}

	if msg.err != nil && followed.reason == "" {
		return s.onPodFailed(msg, followed.restarts)
	}
	if !msg.opened {
		// The pod was deleted before its log opened.
		return nil
	}

	s.ended[msg.pod] = followed.restarts
	delete(s.retries, msg.pod)
	reason := followed.reason
	if reason == "" {
		reason = "container stopped"
	}
	if err := s.event(LogEventPodEnded, LogPodEvent{Pod: msg.pod, Reason: reason}); err != nil {
		return err
	}
	return s.fillSlots()
}

// onPodFailed handles a pod log that failed to open or broke off with an
// error: a refusal ends the stream, anything else is tried again.
func (s *logSession) onPodFailed(msg podMsg, restarts int32) error {
	if k8serrors.IsForbidden(msg.err) || k8serrors.IsUnauthorized(msg.err) {
		_ = s.event(LogEventError, LogNotice{Message: "reading the log of pod " + msg.pod + " was refused: " + msg.err.Error()})
		return errors.New("log refused")
	}
	if msg.opened {
		if err := s.event(LogEventPodEnded, LogPodEvent{Pod: msg.pod, Reason: "log stream failed"}); err != nil {
			return err
		}
	}
	if err := s.retryLater(msg.pod, restarts, msg.err); err != nil {
		return err
	}
	return s.fillSlots()
}

// retryLater tries pod's log again after a backoff, or gives up on its
// container after maxLogRetries failures in a row; a restart of the
// container is followed again either way.
func (s *logSession) retryLater(pod string, restarts int32, cause error) error {
	attempt := s.retries[pod] + 1
	if attempt > maxLogRetries {
		delete(s.retries, pod)
		s.ended[pod] = restarts
		return s.event(LogEventNotice, LogNotice{Message: fmt.Sprintf(
			"reading the log of pod %s failed %d times in a row (%v); it is followed again when its container restarts, or connect again", pod, maxLogRetries, cause)})
	}
	s.retries[pod] = attempt
	s.retrying[pod] = true
	delay := min(logRetryBase<<(attempt-1), logRetryMaxDelay)
	if err := s.event(LogEventNotice, LogNotice{Message: fmt.Sprintf(
		"reading the log of pod %s failed (%v); trying again in %s", pod, cause, delay)}); err != nil {
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			return
		}
		select {
		case s.msgs <- podMsg{kind: podRetry, pod: pod}:
		case <-s.ctx.Done():
		}
	}()
	return nil
}

// fillSlots starts pods that were over the cap while a slot is free.
func (s *logSession) fillSlots() error {
	for _, pod := range s.pods {
		if len(s.active) >= maxLogStreamPods {
			break
		}
		if err := s.consider(pod); err != nil {
			return err
		}
	}
	return nil
}

// event writes one SSE event. It flushes only when no other message waits,
// so that a busy log is sent in larger writes.
func (s *logSession) event(name string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.write(fmt.Sprintf("event: %s\ndata: %s\n\n", name, payload), len(s.msgs) == 0)
}

// write sends text to the client within logWriteTimeout, so a client that
// stopped reading makes it fail and ends the stream.
func (s *logSession) write(text string, flush bool) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(logWriteTimeout))
	if _, err := io.WriteString(s.w, text); err != nil {
		return err
	}
	if flush {
		return s.rc.Flush()
	}
	return nil
}

// follow reads one pod's log and sends it to the session line by line,
// leaving out the lines not after skip (sent already, or older than the
// client asked for; the API server starts on a whole second).
func (s *logSession) follow(ctx context.Context, pod string, start cluster.LogStart, skip time.Time) {
	defer s.wg.Done()
	send := func(msg podMsg) bool {
		select {
		case s.msgs <- msg:
			return true
		case <-s.ctx.Done():
			return false
		}
	}

	stream, err := s.streamer.FollowLog(ctx, s.namespace, pod, s.opts.container, start)
	if err != nil {
		send(podMsg{kind: podDone, pod: pod, err: err})
		return
	}
	// Closing the stream also ends a read that waits for the next line.
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer func() {
		stop()
		_ = stream.Close()
	}()

	if !send(podMsg{kind: podOpened, pod: pod}) {
		return
	}
	err = readLogLines(stream, func(raw string) bool {
		timestamp, line := splitTimestamp(raw)
		if !skip.IsZero() {
			if ts, err := time.Parse(time.RFC3339Nano, timestamp); err == nil && !ts.After(skip) {
				return true
			}
		}
		return send(podMsg{kind: podLine, pod: pod, line: LogLine{Pod: pod, Container: s.opts.container, Timestamp: timestamp, Line: line}})
	})
	if ctx.Err() != nil {
		err = nil
	}
	send(podMsg{kind: podDone, pod: pod, err: err, opened: true})
}

// readLogLines calls emit for each line of r, without its line ending, until
// r ends or emit returns false. A line longer than maxLogLineBytes is cut.
func readLogLines(r io.Reader, emit func(string) bool) error {
	reader := bufio.NewReaderSize(r, 32*1024)
	var line []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if len(chunk) > 0 && len(line) < maxLogLineBytes {
			room := maxLogLineBytes - len(line)
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			line = append(line, chunk...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) > 0 {
					emit(string(line))
				}
				return nil
			}
			return err
		}
		if isPrefix {
			continue
		}
		if !emit(strings.TrimSuffix(string(line), "\r")) {
			return nil
		}
		line = line[:0]
	}
}

// splitTimestamp splits the RFC 3339 timestamp the kubelet puts before each
// line when asked for timestamps.
func splitTimestamp(raw string) (string, string) {
	head, rest, found := strings.Cut(raw, " ")
	if !found {
		head, rest = raw, ""
	}
	if _, err := time.Parse(time.RFC3339Nano, head); err != nil {
		return "", raw
	}
	return head, rest
}

// containerState tells whether pod has the container, whether it has
// started (a log exists), how often it restarted and when the current
// container started (zero when the status does not say).
func containerState(pod *corev1.Pod, container string) (exists, started bool, restarts int32, startedAt time.Time) {
	for _, spec := range pod.Spec.Containers {
		if spec.Name == container {
			exists = true
			break
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container {
			switch {
			case status.State.Running != nil:
				return exists, true, status.RestartCount, status.State.Running.StartedAt.Time
			case status.State.Terminated != nil:
				return exists, true, status.RestartCount, status.State.Terminated.StartedAt.Time
			}
			return exists, false, status.RestartCount, time.Time{}
		}
	}
	return exists, false, 0, time.Time{}
}
