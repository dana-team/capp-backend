package tracking

import (
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Actions serves the revert and accept endpoints of one namespaced kind.
type Actions struct {
	Tracker *Tracker
	Kind    desiredstate.Kind

	// New returns an empty object of the kind.
	New func() client.Object

	// Managed reports whether the API exposes an object. Nil means all.
	Managed func(client.Object) bool

	// Reverted runs after an object was reverted. May be nil.
	Reverted func(c *gin.Context, k8sClient client.Client, object client.Object) error
}

// Register adds the endpoints to the kind's route group.
func (a Actions) Register(rg *gin.RouterGroup) {
	rg.POST("/:name/drift/revert", a.revert)
	rg.POST("/:name/drift/accept", a.accept)
}

// action is a revert or accept request whose preconditions hold.
type action struct {
	k8sClient client.Client
	write     *Write
	live      client.Object // nil when the object does not exist
	drift     *drift.Drift
}

// revert makes the cluster match the desired state. It uses the caller's
// client, so the cluster decides whether the caller may change the object.
func (a Actions) revert(c *gin.Context) {
	request, ok := a.prepare(c)
	if !ok {
		return
	}
	if !request.write.record.Active() {
		apierrors.Respond(c, apierrors.NewNotFound("desired state of "+string(a.Kind), c.Param("name")))
		return
	}
	if request.drift.State == drift.StateInSync {
		c.JSON(http.StatusOK, drift.ActionResponse{Drift: request.drift})
		return
	}
	desired, err := request.write.Desired()
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	object := request.live
	if object == nil {
		object = a.New()
		object.SetNamespace(c.Param("namespace"))
		object.SetName(c.Param("name"))
	}
	if err := desired.Apply(object); err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	if request.live == nil {
		err = request.k8sClient.Create(c.Request.Context(), object)
	} else {
		err = request.k8sClient.Update(c.Request.Context(), object)
	}
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if a.Reverted != nil {
		if err := a.Reverted(c, request.k8sClient, object); err != nil {
			apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("%s reverted, but a follow-up failed: %w", a.Kind, err)))
			return
		}
	}
	c.JSON(http.StatusOK, drift.ActionResponse{Drift: a.Tracker.Inspect(c, request.write.ref, object)})
}

// accept makes the desired state match the cluster. Only the store changes, so
// any caller who can read the object may do it. Accepting that an object is
// gone archives its record instead of deleting it.
func (a Actions) accept(c *gin.Context) {
	request, ok := a.prepare(c)
	if !ok {
		return
	}
	ctx, cancel := a.Tracker.detached(c)
	defer cancel()
	var err error
	if a.managed(request.live) {
		err = request.write.store(ctx, desiredstate.SourceAccept, request.live)
	} else {
		err = request.write.archive(ctx)
	}
	RespondAccepted(c, err, func() *drift.Drift {
		if !request.write.record.Active() {
			return nil
		}
		return a.Tracker.Inspect(c, request.write.ref, request.live)
	})
}

func (a Actions) managed(object client.Object) bool {
	return !isNil(object) && (a.Managed == nil || a.Managed(object))
}

// prepare reads the request, the live object and the record, and checks that
// both still are what the caller saw.
func (a Actions) prepare(c *gin.Context) (*action, bool) {
	if !a.Tracker.Enabled() {
		apierrors.Respond(c, apierrors.NewNotSupported("drift"))
		return nil, false
	}
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return nil, false
	}
	var request drift.ActionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return nil, false
	}
	namespace, name := c.Param("namespace"), c.Param("name")
	live := a.New()
	err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, live)
	if k8serrors.IsNotFound(err) {
		live = nil
	} else if err != nil {
		apierrors.Respond(c, err)
		return nil, false
	}
	write, ok := a.Tracker.Require(c, Ref(c, a.Kind, namespace, name))
	if !ok {
		return nil, false
	}
	// An object the API does not expose, and never did, does not exist here.
	if !a.managed(live) && !write.record.Active() {
		apierrors.Respond(c, apierrors.NewNotFound(string(a.Kind), name))
		return nil, false
	}
	var object any
	if live != nil {
		object = live
	}
	current, ok := write.Confirm(c, object, request)
	if !ok {
		return nil, false
	}
	return &action{k8sClient: k8sClient, write: write, live: live, drift: current}, true
}

// Confirm checks that the object and its record still are what the caller of a
// revert or accept saw. Otherwise it answers 409 with the current drift.
func (w *Write) Confirm(c *gin.Context, live any, request drift.ActionRequest) (*drift.Drift, bool) {
	if isTerminating(live) {
		apierrors.Respond(c, apierrors.NewConflict("the object is being deleted"))
		return nil, false
	}
	current, err := w.tracker.evaluate(w.record, live)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return nil, false
	}
	if current == nil {
		apierrors.Respond(c, apierrors.NewNotFound(string(w.ref.Kind), w.ref.Name))
		return nil, false
	}
	if request.Generation != current.Generation || request.LiveVersion != current.LiveVersion {
		apierrors.Respond(c, apierrors.NewDriftDetected("the object or its desired state changed; review the drift again", current))
		return nil, false
	}
	return current, true
}

// AcceptState records live as the desired state of a Confirmed write. A nil
// live archives the record.
func (w *Write) AcceptState(c *gin.Context, live any) error {
	ctx, cancel := w.tracker.detached(c)
	defer cancel()
	if isNil(live) {
		return w.archive(ctx)
	}
	return w.store(ctx, desiredstate.SourceAccept, live)
}

// Recorded reports whether an active desired state exists.
func (w *Write) Recorded() bool { return w != nil && w.record.Active() }

// RespondAccepted writes the response of an accept.
func RespondAccepted(c *gin.Context, err error, current func() *drift.Drift) {
	switch {
	case err == nil:
		c.JSON(http.StatusOK, drift.ActionResponse{Drift: current()})
	case isConflict(err):
		apierrors.Respond(c, apierrors.NewDriftDetected("the desired state changed; review the drift again", current()))
	default:
		apierrors.Respond(c, apierrors.NewDesiredStateUnavailable(err))
	}
}
