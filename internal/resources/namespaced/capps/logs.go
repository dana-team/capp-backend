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
)

// Variables, not constants, so that tests can make them short.
var (
	logStreamDuration = maxLogStreamDuration
	logHeartbeat      = logHeartbeatInterval
	logRewatch        = logRewatchDelay
)

// logs handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/logs
//
// It answers a text/event-stream that follows the log of every pod of the
// Capp, joining pods as they start, until the client goes away or
// maxLogStreamDuration passes. Query: pod (only this pod), container
// (default: the Capp's container), tailLines (lines of history per pod,
// default 100). Errors found before the stream starts are ordinary JSON
// error responses; later ones are error events.
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

	// The server's write timeout would close the stream after 30 seconds.
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(logStreamDuration + time.Minute))
	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	// nginx (OpenShift routes and most ingresses) buffers a response
	// unless told not to.
	header.Set("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	c.Writer.Flush()

	s := &logSession{
		ctx:       ctx,
		cancel:    cancel,
		w:         c.Writer,
		streamer:  streamer,
		namespace: namespace,
		selector:  selector,
		opts:      opts,
		msgs:      make(chan podMsg, 256),
		active:    map[string]*followedPod{},
		ended:     map[string]int32{},
		noticed:   map[string]bool{},
		pods:      map[string]*corev1.Pod{},
	}
	s.run(podWatch)
}

type logOptions struct {
	pod       string
	container string
	tailLines int64
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
	return opts, nil
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
// stopped).
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
			if podWatch, err = s.watchEnded(ev, open); err != nil {
				if s.ctx.Err() == nil {
					_ = s.event(LogEventError, LogNotice{Message: "watching the Capp's pods failed: " + err.Error()})
				}
				return
			}
		case msg := <-s.msgs:
			err = s.onPodMsg(msg)
		case <-heartbeat.C:
			// An SSE comment; it keeps proxies from closing an idle stream.
			_, err = io.WriteString(s.w, ": keepalive\n\n")
			s.w.Flush()
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
	if _, ok := s.active[pod.Name]; ok {
		return nil
	}
	exists, started, restarts := containerState(pod, s.opts.container)
	if !exists {
		return s.noticeOnce(pod.Name, fmt.Sprintf("pod %s has no container %s", pod.Name, s.opts.container))
	}
	if !started {
		return nil
	}
	tailLines := s.opts.tailLines
	if last, seen := s.ended[pod.Name]; seen {
		if restarts <= last {
			return nil
		}
		// A restarted container has a new log; all of it is new to the
		// client.
		tailLines = maxLogTailLines
	}
	if len(s.active) >= maxLogStreamPods {
		return s.noticeOnce(pod.Name, fmt.Sprintf("pod %s is not followed: the stream follows %d pods already", pod.Name, maxLogStreamPods))
	}
	delete(s.noticed, pod.Name)

	podCtx, cancel := context.WithCancel(s.ctx)
	s.active[pod.Name] = &followedPod{cancel: cancel, restarts: restarts}
	s.wg.Add(1)
	go s.follow(podCtx, pod.Name, tailLines)
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
		return s.event(LogEventLine, msg.line)
	}

	followed := s.active[msg.pod]
	delete(s.active, msg.pod)
	if followed == nil {
		return nil
	}
	followed.cancel()

	if !msg.opened {
		// The log could not be opened, for instance because the container
		// stopped between the pod event and the request. Nothing is
		// recorded, so the next pod event tries again; a refusal is final.
		if k8serrors.IsForbidden(msg.err) || k8serrors.IsUnauthorized(msg.err) {
			_ = s.event(LogEventError, LogNotice{Message: "reading the log of pod " + msg.pod + " was refused: " + msg.err.Error()})
			return errors.New("log refused")
		}
		return nil
	}

	s.ended[msg.pod] = followed.restarts
	reason := followed.reason
	if reason == "" {
		reason = "container stopped"
		if msg.err != nil && s.ctx.Err() == nil {
			reason = "log stream failed"
		}
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if err := s.event(LogEventPodEnded, LogPodEvent{Pod: msg.pod, Reason: reason}); err != nil {
		return err
	}
	// A slot is free: start a pod that was over the cap.
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
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	if len(s.msgs) == 0 {
		s.w.Flush()
	}
	return nil
}

// follow reads one pod's log and sends it to the session line by line.
func (s *logSession) follow(ctx context.Context, pod string, tailLines int64) {
	defer s.wg.Done()
	send := func(msg podMsg) bool {
		select {
		case s.msgs <- msg:
			return true
		case <-s.ctx.Done():
			return false
		}
	}

	stream, err := s.streamer.FollowLog(ctx, s.namespace, pod, s.opts.container, tailLines)
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
// started (a log exists) and how often it restarted.
func containerState(pod *corev1.Pod, container string) (exists, started bool, restarts int32) {
	for _, spec := range pod.Spec.Containers {
		if spec.Name == container {
			exists = true
			break
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container {
			started = status.State.Running != nil || status.State.Terminated != nil
			return exists, started, status.RestartCount
		}
	}
	return exists, false, 0
}
