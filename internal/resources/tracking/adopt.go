package tracking

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AdoptKind is a kind the adopt endpoint covers.
type AdoptKind struct {
	Kind    desiredstate.Kind
	NewList func() client.ObjectList
	Options []client.ListOption
}

// Adopter serves the endpoint that records everything in a namespace that has
// no desired state yet. Run it once per namespace on an existing installation.
type Adopter struct {
	tracker *Tracker
	kinds   []AdoptKind
}

// NewAdopter returns the handler of the adopt endpoint.
func NewAdopter(tracker *Tracker, kinds ...AdoptKind) *Adopter {
	return &Adopter{tracker: tracker, kinds: kinds}
}

// Name returns the handler's identifier.
func (a *Adopter) Name() string { return "drift" }

// RegisterRoutes attaches the adopt route to the cluster router group.
func (a *Adopter) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/namespaces/:namespace/drift/adopt", a.adopt)
}

// adopt handles POST /api/v1/clusters/:cluster/namespaces/:namespace/drift/adopt.
// It never changes an existing desired state. Kinds the caller may not list
// are skipped.
func (a *Adopter) adopt(c *gin.Context) {
	if !a.tracker.Enabled() {
		apierrors.Respond(c, apierrors.NewNotSupported("drift"))
		return
	}
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	if !a.tracker.available(c) {
		return
	}
	response := drift.AdoptResponse{Adopted: []drift.AdoptedItem{}}
	for _, kind := range a.kinds {
		items, err := a.adoptKind(c, k8sClient, kind)
		response.Adopted = append(response.Adopted, items...)
		if err != nil {
			apierrors.Respond(c, err)
			return
		}
	}
	response.Total = len(response.Adopted)
	c.JSON(http.StatusOK, response)
}

func (a *Adopter) adoptKind(c *gin.Context, k8sClient client.Client, kind AdoptKind) ([]drift.AdoptedItem, error) {
	namespace := c.Param("namespace")
	list := kind.NewList()
	options := append([]client.ListOption{client.InNamespace(namespace)}, kind.Options...)
	if err := k8sClient.List(c.Request.Context(), list, options...); k8serrors.IsForbidden(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	objects, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	var adopted []drift.AdoptedItem
	for _, item := range objects {
		object, ok := item.(client.Object)
		if !ok {
			return adopted, fmt.Errorf("unsupported list item type %T", item)
		}
		if isTerminating(object) {
			continue
		}
		recorded, err := a.adoptObject(c, kind.Kind, object)
		if err != nil {
			return adopted, apierrors.NewDesiredStateUnavailable(err)
		}
		if recorded {
			adopted = append(adopted, drift.AdoptedItem{Kind: string(kind.Kind), Name: object.GetName()})
		}
	}
	return adopted, nil
}

func (a *Adopter) adoptObject(c *gin.Context, kind desiredstate.Kind, object client.Object) (bool, error) {
	ctx, cancel := a.tracker.detached(c)
	defer cancel()
	ref := Ref(c, kind, object.GetNamespace(), object.GetName())
	record, err := a.tracker.load(ctx, ref)
	if err != nil || record.Active() {
		return false, err
	}
	w := &Write{tracker: a.tracker, ref: ref, record: record, actor: actorOf(c)}
	err = w.store(ctx, desiredstate.SourceAccept, object)
	if isConflict(err) {
		return false, nil // recorded by another request in the meantime
	}
	return err == nil, err
}

func isConflict(err error) bool { return errors.Is(err, desiredstate.ErrConflict) }
