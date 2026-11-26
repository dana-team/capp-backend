// Package tracking connects the resource handlers to the desired state store.
//
// There is no watcher and no periodic job. Drift is computed only when a
// client reads an object, by comparing the live object with its record.
//
// Writes go to the cluster first and are recorded afterwards, from the object
// the cluster returned. A write the cluster refuses is therefore never
// recorded, and defaults set by the cluster never look like drift.
//
// The store is a backup, so a request that can do without it does. When the
// store fails, reads report the drift as unavailable, and writes change the
// cluster, record nothing and carry a warning. The next read that reaches the
// store shows the difference as drift. Only requests about the desired state
// itself are refused.
//
// A nil *Tracker means that desired state is disabled. All methods of Tracker
// and Write accept a nil receiver and then do nothing.
package tracking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/pkg/api/common"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// saveAttempts bounds how often a save is repeated when another request
// records the same object at the same time.
const saveAttempts = 3

// Tracker reads and writes desired state on behalf of the handlers.
type Tracker struct {
	store   desiredstate.Store
	sealer  *desiredstate.Sealer
	timeout time.Duration
}

// New returns a Tracker. timeout bounds every store operation.
func New(store desiredstate.Store, sealer *desiredstate.Sealer, timeout time.Duration) *Tracker {
	return &Tracker{store: store, sealer: sealer, timeout: timeout}
}

// Enabled reports whether desired state is in use.
func (t *Tracker) Enabled() bool { return t != nil }

// Ref returns the identity of an object in the request's cluster.
func Ref(c *gin.Context, kind desiredstate.Kind, namespace, name string) desiredstate.Ref {
	return desiredstate.Ref{Cluster: c.Param("cluster"), Kind: kind, Namespace: namespace, Name: name}
}

// Objects returns pointers to the items of a typed list.
func Objects[T any, P interface {
	*T
	client.Object
}](items []T) []client.Object {
	objects := make([]client.Object, len(items))
	for i := range items {
		objects[i] = P(&items[i])
	}
	return objects
}

// RespondDeleted writes the response of a delete: 204, or 200 with warnings.
func RespondDeleted(c *gin.Context, warnings []common.Warning) {
	if len(warnings) > 0 {
		c.JSON(http.StatusOK, common.DeleteResponse{Warnings: warnings})
		return
	}
	c.Status(http.StatusNoContent)
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// Inspect returns the drift of one object, or nil when there is nothing to
// compare. live is the object, or nil when it does not exist. Call it only
// after the cluster answered the caller's own read, so that the cluster's
// access rules also decide who may see the desired state.
func (t *Tracker) Inspect(c *gin.Context, ref desiredstate.Ref, live any) *drift.Drift {
	if t == nil || isTerminating(live) {
		return nil
	}
	record, err := t.load(c.Request.Context(), ref)
	if err != nil {
		return unavailable(c, err)
	}
	result, err := t.evaluate(record, live)
	if err != nil {
		return unavailable(c, err)
	}
	return result
}

// InspectList returns the drift of every live object, in the same order, and
// the recorded objects of the namespace that no longer exist.
func (t *Tracker) InspectList(c *gin.Context, kind desiredstate.Kind, namespace string, live []client.Object) ([]*drift.Drift, []drift.Missing) {
	return t.inspectList(c, kind, []string{namespace}, live)
}

// InspectAll is InspectList for a list across namespaces. It reports missing
// objects only for namespaces the caller may list.
func (t *Tracker) InspectAll(c *gin.Context, kind desiredstate.Kind, live []client.Object) ([]*drift.Drift, []drift.Missing) {
	if t == nil || !policy.Enabled(c) {
		// In Kubernetes mode the cluster-wide list succeeded, so the caller may
		// read this kind in every namespace.
		return t.inspectList(c, kind, nil, live)
	}
	namespaces, err := policy.AggregateNamespaces(c)
	if err != nil {
		return allUnavailable(c, err, len(live)), nil
	}
	return t.inspectList(c, kind, namespaces, live)
}

func (t *Tracker) inspectList(c *gin.Context, kind desiredstate.Kind, namespaces []string, live []client.Object) ([]*drift.Drift, []drift.Missing) {
	drifts := make([]*drift.Drift, len(live))
	if t == nil {
		return drifts, nil
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), t.timeout)
	defer cancel()
	records, err := t.store.List(ctx, desiredstate.Filter{Cluster: c.Param("cluster"), Kind: kind, Namespaces: namespaces})
	if err != nil {
		return allUnavailable(c, err, len(live)), nil
	}
	byKey := make(map[client.ObjectKey]*desiredstate.Record, len(records))
	for i := range records {
		byKey[client.ObjectKey{Namespace: records[i].Namespace, Name: records[i].Name}] = &records[i]
	}
	for i, object := range live {
		key := client.ObjectKeyFromObject(object)
		record := byKey[key]
		delete(byKey, key)
		if isTerminating(object) {
			continue
		}
		if drifts[i], err = t.evaluate(record, object); err != nil {
			drifts[i] = unavailable(c, err)
		}
	}
	return drifts, t.missing(c, byKey)
}

func (t *Tracker) missing(c *gin.Context, records map[client.ObjectKey]*desiredstate.Record) []drift.Missing {
	var missing []drift.Missing
	for key, record := range records {
		result, err := t.evaluate(record, nil)
		if err != nil {
			result = unavailable(c, err)
		}
		if result != nil {
			missing = append(missing, drift.Missing{Namespace: key.Namespace, Name: key.Name, Drift: *result})
		}
	}
	slices.SortFunc(missing, func(a, b drift.Missing) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return missing
}

// RecordedNamespaces returns every namespace of the cluster that has a desired
// state, as if it were missing. The caller decides which of them really are.
func (t *Tracker) RecordedNamespaces(c *gin.Context) []drift.Missing {
	_, recorded := t.inspectList(c, desiredstate.KindNamespace, nil, nil)
	return recorded
}

// RespondReadError writes the response for a failed read. When the object does
// not exist but has a desired state, the error carries its drift.
func (t *Tracker) RespondReadError(c *gin.Context, ref desiredstate.Ref, live any, err error) {
	var apiErr *apierrors.APIError
	notFound := k8serrors.IsNotFound(err) || (errors.As(err, &apiErr) && apiErr.Code == apierrors.CodeNotFound)
	if t != nil && notFound {
		if result := t.Inspect(c, ref, live); result != nil && (result.State == drift.StateMissing || result.State == drift.StateChanged) {
			if apiErr == nil {
				apiErr = apierrors.NewNotFound(string(ref.Kind), ref.Name)
			}
			apiErr.Details = map[string]any{"drift": result}
			err = apiErr
		}
	}
	apierrors.Respond(c, err)
}

// ── Writes ────────────────────────────────────────────────────────────────────

// Write is one tracked change. Begin it before the cluster is changed and
// finish it with Save or Delete afterwards.
type Write struct {
	tracker *Tracker
	ref     desiredstate.Ref
	record  *desiredstate.Record // nil when nothing is recorded yet
	actor   string

	// err is set when the record could not be read. The write then goes ahead
	// without being recorded: drift was not checked, and recording the result
	// would hide it.
	err error
}

// Begin reads the record of ref. It never fails: without the store, the write
// goes ahead and is not recorded.
func (t *Tracker) Begin(c *gin.Context, ref desiredstate.Ref) *Write {
	if t == nil {
		return nil
	}
	record, err := t.load(c.Request.Context(), ref)
	return &Write{tracker: t, ref: ref, record: record, actor: actorOf(c), err: err}
}

// Require is Begin for requests that need the record, such as a revert. When
// the store cannot be read it answers 503 and returns false.
func (t *Tracker) Require(c *gin.Context, ref desiredstate.Ref) (*Write, bool) {
	write := t.Begin(c, ref)
	if write != nil && write.err != nil {
		apierrors.Respond(c, apierrors.NewDesiredStateUnavailable(write.err))
		return nil, false
	}
	return write, true
}

// available answers 503 and returns false when the store cannot be reached.
func (t *Tracker) available(c *gin.Context) bool {
	ctx, cancel := context.WithTimeout(c.Request.Context(), t.timeout)
	defer cancel()
	if err := t.store.Ping(ctx); err != nil {
		apierrors.Respond(c, apierrors.NewDesiredStateUnavailable(err))
		return false
	}
	return true
}

// CheckDrift decides how an update treats an object that has drifted. Without
// the onDrift parameter it answers 409 with the differences. With the
// liveVersion or generation parameters, onDrift applies only to the drift they
// name, and a drift that changed since answers 409 again. It returns
// revert=true when the update must start from the desired state, and ok=false
// when it already wrote a response. When the record cannot be read or compared,
// the update starts from the live object; only an explicit revert is refused.
func (w *Write) CheckDrift(c *gin.Context, live any) (revert, ok bool) {
	if w == nil {
		return false, true
	}
	onDrift := c.Query("onDrift")
	switch onDrift {
	case "", drift.OnDriftAccept, drift.OnDriftRevert:
	default:
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("onDrift must be %q or %q", drift.OnDriftAccept, drift.OnDriftRevert)))
		return false, false
	}
	shown, err := shownDriftOf(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return false, false
	}
	var current *drift.Drift
	if w.err == nil && w.record.Active() {
		current, w.err = w.tracker.evaluate(w.record, live)
	}
	if w.err != nil && onDrift == drift.OnDriftRevert {
		apierrors.Respond(c, apierrors.NewDesiredStateUnavailable(w.err))
		return false, false
	}
	if current == nil || current.State != drift.StateChanged {
		return false, true
	}
	if onDrift == "" {
		apierrors.Respond(c, apierrors.NewDriftDetected(
			"the object differs from its desired state; repeat the request with onDrift=accept to keep the live changes, or onDrift=revert to discard them", current))
		return false, false
	}
	// Somebody changed the object or its desired state after the caller decided:
	// accepting would record a change nobody saw, reverting would discard one.
	if !shown.matches(current) {
		apierrors.Respond(c, apierrors.NewDriftDetected(
			"the object or its desired state changed since its drift was shown; review the drift again", current))
		return false, false
	}
	return onDrift == drift.OnDriftRevert, true
}

// shownDrift is the drift an update's onDrift decision is about, as the
// liveVersion and generation query parameters name it. Both are optional, so
// that a client may decide for whatever drift there is, as cappctl --on-drift
// does.
type shownDrift struct {
	liveVersion string
	generation  *int64
}

func shownDriftOf(c *gin.Context) (shownDrift, error) {
	shown := shownDrift{liveVersion: c.Query("liveVersion")}
	if value := c.Query("generation"); value != "" {
		generation, err := strconv.ParseInt(value, 10, 64)
		if err != nil || generation < 0 {
			return shownDrift{}, errors.New("generation must be a non-negative integer")
		}
		shown.generation = &generation
	}
	return shown, nil
}

// matches reports whether current is the drift that was shown.
func (s shownDrift) matches(current *drift.Drift) bool {
	return (s.liveVersion == "" || s.liveVersion == current.LiveVersion) &&
		(s.generation == nil || *s.generation == current.Generation)
}

// ResolveDrift is CheckDrift for a Kubernetes object. When the caller chose to
// revert, it changes object to its desired state and returns reverted=true.
func (w *Write) ResolveDrift(c *gin.Context, object client.Object) (reverted, ok bool) {
	revert, ok := w.CheckDrift(c, object)
	if !ok || !revert {
		return false, ok
	}
	state, err := w.Desired()
	if err == nil {
		err = state.Apply(object)
	}
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return false, false
	}
	return true, true
}

// Desired returns the recorded state. The record must be active.
func (w *Write) Desired() (desiredstate.State, error) {
	return desiredstate.Open(w.record, w.tracker.sealer)
}

// Save records object, as returned by the cluster, as the desired state.
func (w *Write) Save(c *gin.Context, reader client.Reader, object client.Object) []common.Warning {
	if w == nil {
		return nil
	}
	return w.SaveState(c, object, func(ctx context.Context) (any, error) {
		return refetch(ctx, reader, object)
	})
}

// SaveState records state as the desired state. live reads the current state
// again when another request recorded the object in the meantime.
func (w *Write) SaveState(c *gin.Context, state any, live func(context.Context) (any, error)) []common.Warning {
	if w == nil {
		return nil
	}
	err := w.err
	if err == nil {
		err = w.save(c, desiredstate.SourceWrite, state, live)
	}
	if err != nil {
		return warning(c, err)
	}
	return nil
}

// SaveCurrent reads the current state and records it as the desired state. A
// nil state means the object is gone and leaves the record as it is.
func (w *Write) SaveCurrent(c *gin.Context, live func(context.Context) (any, error)) []common.Warning {
	if w == nil {
		return nil
	}
	if w.err != nil {
		return warning(c, w.err)
	}
	ctx, cancel := w.tracker.detached(c)
	state, err := live(ctx)
	cancel()
	if err != nil {
		return warning(c, err)
	}
	if isNil(state) {
		return nil
	}
	return w.SaveState(c, state, live)
}

// Delete removes the record of a deleted object.
func (w *Write) Delete(c *gin.Context) []common.Warning {
	if w == nil {
		return nil
	}
	if w.err != nil {
		return warning(c, w.err)
	}
	if w.record == nil {
		return nil
	}
	ctx, cancel := w.tracker.detached(c)
	defer cancel()
	err := w.tracker.store.Delete(ctx, w.ref, w.record.Generation)
	// On a conflict somebody recorded the name again after the delete.
	if err != nil && !errors.Is(err, desiredstate.ErrConflict) {
		return warning(c, err)
	}
	return nil
}

// DeleteNamespace removes every record of a deleted namespace.
func (t *Tracker) DeleteNamespace(c *gin.Context, namespace string) []common.Warning {
	if t == nil {
		return nil
	}
	ctx, cancel := t.detached(c)
	defer cancel()
	if err := t.store.DeleteNamespace(ctx, c.Param("cluster"), namespace); err != nil {
		return warning(c, err)
	}
	return nil
}

// Record stores an object that was created outside a single Write, such as the
// copies a migration makes.
func (t *Tracker) Record(c *gin.Context, ref desiredstate.Ref, reader client.Reader, object client.Object) []common.Warning {
	if t == nil {
		return nil
	}
	ctx, cancel := t.detached(c)
	record, err := t.load(ctx, ref)
	cancel()
	if err != nil {
		return warning(c, err)
	}
	w := &Write{tracker: t, ref: ref, record: record, actor: actorOf(c)}
	return w.Save(c, reader, object)
}

// Forget removes the record of an object that was deleted outside a single
// Write.
func (t *Tracker) Forget(c *gin.Context, ref desiredstate.Ref) []common.Warning {
	if t == nil {
		return nil
	}
	ctx, cancel := t.detached(c)
	record, err := t.load(ctx, ref)
	cancel()
	if err != nil {
		return warning(c, err)
	}
	return (&Write{tracker: t, ref: ref, record: record}).Delete(c)
}

func (w *Write) save(c *gin.Context, source desiredstate.Source, state any, live func(context.Context) (any, error)) error {
	ctx, cancel := w.tracker.detached(c)
	defer cancel()
	var err error
	for range saveAttempts {
		if err = w.store(ctx, source, state); !errors.Is(err, desiredstate.ErrConflict) {
			return err
		}
		// Two requests changed the object. Their saves may arrive in another
		// order than their cluster writes, so record what is live now.
		if w.record, err = w.tracker.load(ctx, w.ref); err != nil {
			return err
		}
		if state, err = live(ctx); k8serrors.IsNotFound(err) || (err == nil && isNil(state)) {
			return nil
		} else if err != nil {
			return err
		}
	}
	return err
}

func (w *Write) store(ctx context.Context, source desiredstate.Source, live any) error {
	state, err := desiredstate.StateOf(live)
	if err != nil {
		return err
	}
	if w.record.Active() {
		if recorded, err := w.Desired(); err == nil && recorded.Equal(state) {
			return nil
		}
	}
	desired, err := state.Seal(w.ref, w.tracker.sealer)
	if err != nil {
		return err
	}
	return w.put(ctx, source, desired, nil)
}

// archive keeps the desired state but takes it out of drift detection.
func (w *Write) archive(ctx context.Context) error {
	now := time.Now().UTC()
	return w.put(ctx, desiredstate.SourceAccept, w.record.Desired, &now)
}

func (w *Write) put(ctx context.Context, source desiredstate.Source, desired desiredstate.Desired, archivedAt *time.Time) error {
	var expected int64
	if w.record != nil {
		expected = w.record.Generation
	}
	record := &desiredstate.Record{
		ID: w.ref.ID(), Cluster: w.ref.Cluster, Kind: w.ref.Kind, Namespace: w.ref.Namespace, Name: w.ref.Name,
		Desired: desired, Generation: expected + 1, ArchivedAt: archivedAt,
		Source: source, UpdatedBy: w.actor, UpdatedAt: time.Now().UTC(),
	}
	if err := w.tracker.store.Save(ctx, record, expected); err != nil {
		return err
	}
	w.record = record
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// load returns the record of ref, or nil when none exists.
func (t *Tracker) load(ctx context.Context, ref desiredstate.Ref) (*desiredstate.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	record, err := t.store.Get(ctx, ref)
	if errors.Is(err, desiredstate.ErrNotFound) {
		return nil, nil
	}
	return record, err
}

// evaluate compares live, or nil when the object does not exist, with record.
// It returns nil when neither takes part in drift detection.
func (t *Tracker) evaluate(record *desiredstate.Record, live any) (*drift.Drift, error) {
	result := &drift.Drift{}
	if record != nil {
		result.Generation = record.Generation
	}
	if record.Active() {
		result.UpdatedBy = record.UpdatedBy
		result.UpdatedAt = record.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if isNil(live) {
		if !record.Active() {
			return nil, nil
		}
		result.State = drift.StateMissing
		return result, nil
	}
	state, err := desiredstate.StateOf(live)
	if err != nil {
		return nil, err
	}
	result.LiveVersion = state.Version(t.sealer)
	if !record.Active() {
		result.State = drift.StateUnmanaged
		return result, nil
	}
	desired, err := desiredstate.Open(record, t.sealer)
	if err != nil {
		return nil, err
	}
	result.State = drift.StateInSync
	if result.Fields = desiredstate.Compare(record.Kind, desired, state); len(result.Fields) > 0 {
		result.State = drift.StateChanged
	}
	return result, nil
}

// detached returns a context for recording a finished cluster write. It
// outlives the request: a client that disconnects must not leave the write
// unrecorded.
func (t *Tracker) detached(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.Request.Context()), t.timeout)
}

func refetch(ctx context.Context, reader client.Reader, object client.Object) (client.Object, error) {
	// A new object, because decoding into a used one merges its maps.
	fresh, ok := reflect.New(reflect.TypeOf(object).Elem()).Interface().(client.Object)
	if !ok {
		return nil, fmt.Errorf("objects of type %T cannot be read again", object)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func isNil(live any) bool {
	if live == nil {
		return true
	}
	value := reflect.ValueOf(live)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// isTerminating reports whether live is being deleted, which is not drift.
func isTerminating(live any) bool {
	if isNil(live) {
		return false
	}
	object, ok := live.(client.Object)
	return ok && !object.GetDeletionTimestamp().IsZero()
}

func actorOf(c *gin.Context) string {
	value, _ := c.Get(string(middleware.CredentialKey))
	credential, _ := value.(auth.ClusterCredential)
	return credential.ImpersonateUser
}

func unavailable(c *gin.Context, err error) *drift.Drift {
	_ = c.Error(fmt.Errorf("desired state: %w", err))
	return &drift.Drift{State: drift.StateUnavailable}
}

func allUnavailable(c *gin.Context, err error, count int) []*drift.Drift {
	result := unavailable(c, err)
	return slices.Repeat([]*drift.Drift{result}, count)
}

func warning(c *gin.Context, err error) []common.Warning {
	_ = c.Error(fmt.Errorf("desired state: %w", err))
	return []common.Warning{{
		Code:    apierrors.CodeDesiredStateFailed,
		Message: "the change was made but not backed up, because its desired state could not be recorded; it will show as drift",
	}}
}
