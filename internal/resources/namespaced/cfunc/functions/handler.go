package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cfuncsource"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/pools"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BuildRequestAnnotation records, on a package that is built from source,
// the code and build settings a build was last requested for. Fission starts
// a build again only when the package status is set to pending, which is a
// second write after the code; the annotation is written last, so a request
// repeated after a failure between the two still asks for the build.
const BuildRequestAnnotation = "cfunc.dana.io/build-requested"

// CodeAnnotation records, on the Function, the unbuilt code its package
// reference was last pointed at. See pointAtCode.
const CodeAnnotation = "cfunc.dana.io/code"

// writeMargin is the time a handler keeps for its cluster writes and its
// response after a slow call to the git server or the router.
const writeMargin = 30 * time.Second

// extendWriteDeadline gives the response of this request d from now. The
// server's write timeout (server.writeTimeoutSeconds, 30 by default) is
// shorter than a clone or a function call can take, and when it passes the
// client gets a closed connection although the handler went on and wrote to
// the cluster. A writer that cannot move its deadline keeps the server's.
func extendWriteDeadline(c *gin.Context, d time.Duration) {
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(d))
}

// CodeFetcher reads the code of a function from the git server.
type CodeFetcher interface {
	CheckURL(repoURL string) error
	Fetch(ctx context.Context, repoURL, ref, folder string) (*cfuncsource.Archive, error)
}

// Handler implements resources.ResourceHandler for cfunc functions.
type Handler struct {
	settings   Settings
	fetcher    CodeFetcher
	routerURLs map[string]string
	testClient *http.Client
}

// New returns a ready-to-use function Handler. It fails when the git base
// URL does not parse, which config.Validate already rejects, so this is a
// startup-time error.
func New(cfg config.CfuncConfig, clusters []config.ClusterConfig) (*Handler, error) {
	fetcher, err := cfuncsource.New(cfg.Functions.Git)
	if err != nil {
		return nil, err
	}
	routerURLs := make(map[string]string, len(clusters))
	for _, cluster := range clusters {
		if cluster.CfuncRouterURL != "" {
			routerURLs[cluster.Name] = cluster.CfuncRouterURL
		}
	}
	return &Handler{
		settings:   Settings{Functions: cfg.Functions, AllowCustomBuild: cfg.AllowCustomBuild},
		fetcher:    fetcher,
		routerURLs: routerURLs,
		testClient: newTestClient(),
	}, nil
}

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "cfuncfunctions" }

// RegisterRoutes attaches all function routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Cluster-scoped list (all namespaces).
	rg.GET("/cfunc/functions", h.listAll)

	// Namespace-scoped operations.
	ns := rg.Group("/namespaces/:namespace/cfunc/functions")
	ns.GET("", h.list)
	ns.POST("", h.create)
	ns.GET("/:name", h.get)
	ns.PUT("/:name", h.update)
	ns.DELETE("/:name", h.delete)
	ns.POST("/:name/test", h.test)
	ns.GET("/:name/logs", h.logs)
}

// listAll handles GET /api/v1/clusters/:cluster/cfunc/functions
// Lists functions across all namespaces visible to the caller.
func (h *Handler) listAll(c *gin.Context) {
	var (
		functions    fissionv1.FunctionList
		packages     fissionv1.PackageList
		httpTriggers fissionv1.HTTPTriggerList
		timeTriggers fissionv1.TimeTriggerList
	)
	if !namespaced.ListAllTogether(c, &functions, &packages, &httpTriggers, &timeTriggers) {
		return
	}

	h.respondList(c, functions, packages, httpTriggers, timeTriggers)
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	var (
		functions    fissionv1.FunctionList
		packages     fissionv1.PackageList
		httpTriggers fissionv1.HTTPTriggerList
		timeTriggers fissionv1.TimeTriggerList
	)
	if err := listTogether(c.Request.Context(), k8sClient, client.InNamespace(c.Param("namespace")),
		&functions, &packages, &httpTriggers, &timeTriggers); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, functions, packages, httpTriggers, timeTriggers)
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	objects, ok := h.load(c, k8sClient)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, FromK8s(objects, h.routerURLs[c.Param("cluster")], h.settings))
}

// create handles POST /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions
//
// The objects are written in an order that a repeated request can finish:
// the package, then the function, then the triggers. A package that a failed
// create left behind has the function's name, so the next create adopts it.
func (h *Handler) create(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	ctx := c.Request.Context()

	var req FunctionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}
	if !h.validate(c, req, namespace) {
		return
	}
	extendWriteDeadline(c, cfuncsource.FetchTimeout+writeMargin)

	var current fissionv1.Function
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: req.Name}, &current)
	if err == nil {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("function %s already exists; update it with PUT", req.Name)))
		return
	}
	if !k8serrors.IsNotFound(err) {
		apierrors.Respond(c, err)
		return
	}

	pool, ok := h.pool(c, k8sClient, namespace, req.Pool)
	if !ok {
		return
	}
	code, ok := h.fetch(c, req)
	if !ok {
		return
	}

	// Triggers can name the function before it exists: ones that a delete
	// with orphans left behind, or ones made with the fission CLI. They are
	// this function's triggers, so they are replaced like on an update and
	// not reported as taken by another function.
	existing, ok := h.triggers(c, k8sClient, namespace, req.Name)
	if !ok {
		return
	}
	var leftover fissionv1.Package
	switch err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: req.Name}, &leftover); {
	case err == nil:
		if !managedPackage(&leftover) {
			apierrors.Respond(c, foreignPackageConflict(req.Name))
			return
		}
		existing.Package = &leftover
	case !k8serrors.IsNotFound(err):
		apierrors.Respond(c, err)
		return
	}

	wanted, err := ToK8s(req, existing, namespace, pool, code, h.settings)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	if err := h.applyPackage(ctx, k8sClient, existing.Package, wanted.Package, nil); err != nil {
		apierrors.Respond(c, err)
		return
	}
	wanted.Function.Spec.Package.PackageRef.ResourceVersion = wanted.Package.ResourceVersion
	if wanted.Package.Spec.Source.IsEmpty() {
		setCodeAnnotation(wanted.Function, codeHash(wanted.Package))
	}
	if err := k8sClient.Create(ctx, wanted.Function); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.finish(c, k8sClient, http.StatusCreated, existing, wanted)
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
// It performs a full replacement of the request fields (not a patch): the
// code is cloned again, and a trigger that the body does not list is deleted.
func (h *Handler) update(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var req FunctionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}
	// The URL param is authoritative: every object of the function is named
	// after it.
	req.Name = name
	if !h.validate(c, req, namespace) {
		return
	}
	extendWriteDeadline(c, cfuncsource.FetchTimeout+writeMargin)

	existing, ok := h.load(c, k8sClient)
	if !ok {
		return
	}
	if executor := existing.Function.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType; executor != "" && executor != fissionv1.ExecutorTypePoolmgr {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf(
			"function %s runs on the %s executor and was not created through this API, which manages pool manager functions only; delete it and create it again",
			name, executor)))
		return
	}
	// The package this API manages is named after the function. A function
	// made outside this API can name another package, and that package can
	// be gone, so the lookup by the function's name does not depend on what
	// the function references.
	if existing.Package == nil || existing.Package.Name != name {
		existing.Package = nil
		var own fissionv1.Package
		switch err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &own); {
		case err == nil:
			existing.Package = &own
		case !k8serrors.IsNotFound(err):
			apierrors.Respond(c, err)
			return
		}
	}
	if existing.Package != nil && !managedPackage(existing.Package) {
		apierrors.Respond(c, foreignPackageConflict(name))
		return
	}

	pool, ok := h.pool(c, k8sClient, namespace, req.Pool)
	if !ok {
		return
	}
	code, ok := h.fetch(c, req)
	if !ok {
		return
	}

	wanted, err := ToK8s(req, existing, namespace, pool, code, h.settings)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	if err := h.applyPackage(ctx, k8sClient, existing.Package, wanted.Package, existing.Function); err != nil {
		apierrors.Respond(c, err)
		return
	}
	behind := pointAtCode(existing.Function, wanted.Function, wanted.Package)
	if behind || !apiequality.Semantic.DeepEqual(existing.Function.Spec, wanted.Function.Spec) {
		if err := updateOverStatusWrites(ctx, k8sClient, wanted.Function); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	h.finish(c, k8sClient, http.StatusOK, existing, wanted)
}

// delete handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name
//
// The function is deleted last, so a delete that fails part of the way can
// be sent again: the function is still there to find the rest by.
func (h *Handler) delete(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	ctx := c.Request.Context()

	objects, ok := h.load(c, k8sClient)
	if !ok {
		return
	}

	for i := range objects.HTTPTriggers {
		if err := client.IgnoreNotFound(k8sClient.Delete(ctx, &objects.HTTPTriggers[i])); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}
	for i := range objects.TimeTriggers {
		if err := client.IgnoreNotFound(k8sClient.Delete(ctx, &objects.TimeTriggers[i])); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}
	// Only the package this API made for the function goes with it. A
	// package of another name, or one of this name that was made another
	// way, can hold the code of other functions.
	if pkg := objects.Package; pkg != nil && pkg.Name == objects.Function.Name && managedPackage(pkg) {
		if err := client.IgnoreNotFound(k8sClient.Delete(ctx, pkg)); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}
	if err := k8sClient.Delete(ctx, objects.Function); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// validate runs the checks that need no cluster and no clone, and writes a
// 400 response when one fails.
func (h *Handler) validate(c *gin.Context, req FunctionRequest, namespace string) bool {
	err := Validate(req, namespace, h.settings)
	if err == nil {
		err = h.fetcher.CheckURL(req.Source.URL)
	}
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return false
	}
	return true
}

// pool reads the function's pool and writes a 400 response when a function
// cannot run in it.
func (h *Handler) pool(c *gin.Context, k8sClient client.Client, namespace, name string) (*fissionv1.Environment, bool) {
	var env fissionv1.Environment
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &env); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf(
				"pool %s does not exist in namespace %s; a function runs in a pool of its own namespace", name, namespace)))
			return nil, false
		}
		apierrors.Respond(c, err)
		return nil, false
	}
	if env.Spec.Version < pools.EnvironmentVersion {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf(
			"pool %s is a version %d Environment that was not created through this API; a function cannot load there", name, env.Spec.Version)))
		return nil, false
	}
	return &env, true
}

// fetch clones the function's code and writes the error response when that
// fails: 400 for what the request names, 503 when the backend has no free
// clone slot, 502 for the git server.
func (h *Handler) fetch(c *gin.Context, req FunctionRequest) (*cfuncsource.Archive, bool) {
	code, err := h.fetcher.Fetch(c.Request.Context(), req.Source.URL, req.Source.Ref, req.Source.Path)
	if err != nil {
		var inputErr *cfuncsource.InputError
		if errors.As(err, &inputErr) {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return nil, false
		}
		if errors.Is(err, cfuncsource.ErrBusy) {
			apierrors.Respond(c, apierrors.NewCfuncSourceBusy(err))
			return nil, false
		}
		apierrors.Respond(c, apierrors.NewCfuncSourceFailed(err))
		return nil, false
	}
	return code, true
}

// function reads the Function named by the URL and writes the error
// response when it is missing or the read fails.
func (h *Handler) function(c *gin.Context, k8sClient client.Client) (*fissionv1.Function, bool) {
	name := c.Param("name")
	var fn fissionv1.Function
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: c.Param("namespace"), Name: name}, &fn); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Function", name))
			return nil, false
		}
		apierrors.Respond(c, err)
		return nil, false
	}
	return &fn, true
}

// triggers lists the triggers of the namespace that name the function. It
// writes the error response and returns false when a list fails.
func (h *Handler) triggers(c *gin.Context, k8sClient client.Client, namespace, function string) (*Objects, bool) {
	var (
		httpTriggers fissionv1.HTTPTriggerList
		timeTriggers fissionv1.TimeTriggerList
	)
	if err := listTogether(c.Request.Context(), k8sClient, client.InNamespace(namespace), &httpTriggers, &timeTriggers); err != nil {
		apierrors.Respond(c, err)
		return nil, false
	}
	return &Objects{
		HTTPTriggers: triggersOf(httpTriggers.Items, function, httpTriggerRef),
		TimeTriggers: triggersOf(timeTriggers.Items, function, timeTriggerRef),
	}, true
}

// listTogether runs independent lists at the same time and returns the first
// error.
func listTogether(ctx context.Context, k8sClient client.Client, option client.ListOption, lists ...client.ObjectList) error {
	group, ctx := errgroup.WithContext(ctx)
	for _, list := range lists {
		group.Go(func() error { return k8sClient.List(ctx, list, option) })
	}
	return group.Wait()
}

// load reads the function named by the URL and the objects that belong to
// it. It writes the error response and returns false when the function is
// missing or a read fails.
func (h *Handler) load(c *gin.Context, k8sClient client.Client) (*Objects, bool) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	fn, ok := h.function(c, k8sClient)
	if !ok {
		return nil, false
	}
	objects := &Objects{Function: fn}

	if packageName := fn.Spec.Package.PackageRef.Name; packageName != "" {
		var pkg fissionv1.Package
		switch err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: packageName}, &pkg); {
		case err == nil:
			objects.Package = &pkg
		case !k8serrors.IsNotFound(err):
			apierrors.Respond(c, err)
			return nil, false
		}
	}

	triggers, ok := h.triggers(c, k8sClient, namespace, name)
	if !ok {
		return nil, false
	}
	objects.HTTPTriggers, objects.TimeTriggers = triggers.HTTPTriggers, triggers.TimeTriggers
	return objects, true
}

// pointAtCode sets, on wanted, the package reference and the code
// annotation the Function must have for the code in pkg, and reports whether
// that changed anything.
//
// The reference's resource version is what tells Fission to load a package
// again: a new one makes the executor drop the function's pods. Who moves it
// depends on the code:
//
//   - Code that is not built is ready as soon as the package is written, so
//     this API moves the reference. It does so only when the Function is
//     behind the code, which it reads from the code annotation and does not
//     remember from the request: a request whose package write succeeded and
//     whose Function write failed leaves the Function behind, and the next
//     one repairs it. An unchanged PUT changes nothing.
//   - Code that is built is not ready until the build ends, and Fission's
//     fetcher refuses a package whose build is pending. Fission's builder
//     moves the reference itself when the build succeeds, so this API
//     leaves it alone: moving it at once would only drop the function's
//     pods earlier. It removes a code annotation from an earlier unbuilt
//     version, so that a later change back is seen as one.
//
// This does not keep the last good build serving through a build that
// fails. Measured on Fission v1.27.0: the builder writes the build result to
// the Function's status, the executor keys its pods on the Function's
// resource version and so drops them, and a new pod cannot fetch a package
// whose build failed. The function then answers 500 until a build succeeds.
// One package holds both the code and the build state, so only a package
// for each build could change that.
func pointAtCode(existing, wanted *fissionv1.Function, pkg *fissionv1.Package) bool {
	recorded, isRecorded := existing.Annotations[CodeAnnotation]
	if !pkg.Spec.Source.IsEmpty() {
		if isRecorded {
			delete(wanted.Annotations, CodeAnnotation)
		}
		return isRecorded
	}
	code := codeHash(pkg)
	if isRecorded && recorded == code {
		return false
	}
	wanted.Spec.Package.PackageRef.ResourceVersion = pkg.ResourceVersion
	setCodeAnnotation(wanted, code)
	return true
}

func setCodeAnnotation(fn *fissionv1.Function, code string) {
	if fn.Annotations == nil {
		fn.Annotations = map[string]string{}
	}
	fn.Annotations[CodeAnnotation] = code
}

// ownerOf returns the owner reference that makes Kubernetes delete an object
// together with fn, which covers a function deleted outside this API.
func ownerOf(fn *fissionv1.Function) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: fissionv1.SchemeGroupVersion.String(),
		Kind:       "Function",
		Name:       fn.Name,
		UID:        fn.UID,
	}}
}

// codeHash identifies the code a package runs: the source, build command
// and pool for code that is built, and the deployment archive for code that
// is not. The deployment archive of built code is the builder's output and
// is left out.
func codeHash(pkg *fissionv1.Package) string {
	hash := sha256.New()
	hash.Write(pkg.Spec.Source.Literal)
	hash.Write([]byte{0})
	hash.Write([]byte(pkg.Spec.BuildCommand))
	hash.Write([]byte{0})
	hash.Write([]byte(pkg.Spec.Environment.Name))
	if pkg.Spec.Source.IsEmpty() {
		hash.Write([]byte{0})
		hash.Write(pkg.Spec.Deployment.Literal)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// applyPackage creates or updates the package and leaves the stored object
// in wanted. owner is nil until the function exists.
//
// A new package is built by Fission by itself. A package that already exists
// is built again only when its status is set to pending, so new source needs
// three writes: the code, the status, and then the build-request annotation
// that marks the request as made.
func (h *Handler) applyPackage(ctx context.Context, k8sClient client.Client, existing, wanted *fissionv1.Package, owner *fissionv1.Function) error {
	fromSource := !wanted.Spec.Source.IsEmpty()
	request := ""
	if fromSource {
		request = codeHash(wanted)
		wanted.Annotations[BuildRequestAnnotation] = request
	} else {
		delete(wanted.Annotations, BuildRequestAnnotation)
	}
	if owner != nil {
		wanted.OwnerReferences = ownerOf(owner)
	}

	if existing == nil {
		return k8sClient.Create(ctx, wanted)
	}

	// A failed build is requested again even for the same code: the cause
	// can be outside it, such as a registry the builder could not reach.
	needsBuild := fromSource && (existing.Annotations[BuildRequestAnnotation] != request ||
		existing.Status.BuildStatus == fissionv1.BuildStatusFailed)
	if needsBuild {
		if previous, ok := existing.Annotations[BuildRequestAnnotation]; ok {
			wanted.Annotations[BuildRequestAnnotation] = previous
		} else {
			delete(wanted.Annotations, BuildRequestAnnotation)
		}
	}
	if !samePackage(existing, wanted) {
		if err := updatePackage(ctx, k8sClient, wanted, fromSource); err != nil {
			return err
		}
	}
	if !fromSource {
		return clearBuildState(ctx, k8sClient, wanted)
	}
	if !needsBuild {
		return nil
	}

	// Fission's builder writes to the package at the same time, so these two
	// writes are patches of their own fields, which cannot conflict with it.
	base := wanted.DeepCopy()
	wanted.Status.BuildStatus = fissionv1.BuildStatusPending
	wanted.Status.BuildLog = ""
	if err := k8sClient.Status().Patch(ctx, wanted, client.MergeFrom(base)); err != nil {
		return err
	}
	base = wanted.DeepCopy()
	wanted.Annotations[BuildRequestAnnotation] = request
	return k8sClient.Patch(ctx, wanted, client.MergeFrom(base))
}

// updateOverStatusWrites updates obj and, on a conflict, applies the same
// update on the stored version. Fission's controllers write the status of a
// Function and of a trigger at any time, and they bump the resource version
// that was read before the clone, which takes seconds. The spec is what
// this request wants in full, so it is written over the stored one.
func updateOverStatusWrites(ctx context.Context, k8sClient client.Client, obj client.Object) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := k8sClient.Update(ctx, obj)
		if !k8serrors.IsConflict(err) {
			return err
		}
		stored, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return err
		}
		if getErr := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), stored); getErr != nil {
			return getErr
		}
		obj.SetResourceVersion(stored.GetResourceVersion())
		return err
	})
}

// updatePackage writes the wanted package. Fission's builder writes to a
// package while it builds, so an update soon after a create or another
// update meets a conflict. It is then applied again on the stored object:
// the fields this API owns are laid over it, and what the builder owns (the
// status, and the deployment archive of built code) is kept.
func updatePackage(ctx context.Context, k8sClient client.Client, wanted *fissionv1.Package, fromSource bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := k8sClient.Update(ctx, wanted)
		if !k8serrors.IsConflict(err) {
			return err
		}
		var stored fissionv1.Package
		if getErr := k8sClient.Get(ctx, client.ObjectKeyFromObject(wanted), &stored); getErr != nil {
			return getErr
		}
		if fromSource {
			wanted.Spec.Deployment = stored.Spec.Deployment
		}
		wanted.Status = stored.Status
		wanted.ResourceVersion = stored.ResourceVersion
		return err
	})
}

// managedPackage reports whether this API made the package. Only such a
// package is taken over or replaced: a package of the same name that was
// made another way can hold the code of other functions.
func managedPackage(pkg *fissionv1.Package) bool {
	_, ok := pkg.Annotations[SourceURLAnnotation]
	return ok
}

func foreignPackageConflict(name string) *apierrors.APIError {
	return apierrors.NewConflict(fmt.Sprintf(
		"a Package named %s exists and was not created through this API, so its code is not replaced; delete it or use another function name", name))
}

// clearBuildState marks a package that is no longer built from source as
// one that needs no build. Fission loads a package only when its build
// state is succeeded or none, so the failed or pending state of an earlier
// source build would keep the new code from loading. The state is read from
// the stored object, so a repeated request does this again after a failure.
func clearBuildState(ctx context.Context, k8sClient client.Client, pkg *fissionv1.Package) error {
	if pkg.Status.BuildStatus == "" || pkg.Status.BuildStatus == fissionv1.BuildStatusNone {
		return nil
	}
	base := pkg.DeepCopy()
	pkg.Status.BuildStatus = fissionv1.BuildStatusNone
	pkg.Status.BuildLog = ""
	return k8sClient.Status().Patch(ctx, pkg, client.MergeFrom(base))
}

func samePackage(existing, wanted *fissionv1.Package) bool {
	return apiequality.Semantic.DeepEqual(existing.Spec, wanted.Spec) &&
		maps.Equal(existing.Annotations, wanted.Annotations) &&
		apiequality.Semantic.DeepEqual(existing.OwnerReferences, wanted.OwnerReferences)
}

// finish does what needs the stored function: it gives the package and the
// triggers their owner, makes the triggers match the request, and writes the
// response. The function and its code are saved by now, so a failure here is
// reported as a partial one that the same request, sent as a PUT, completes.
func (h *Handler) finish(c *gin.Context, k8sClient client.Client, status int, existing, wanted *Objects) {
	ctx := c.Request.Context()
	fn := wanted.Function

	if !apiequality.Semantic.DeepEqual(wanted.Package.OwnerReferences, ownerOf(fn)) {
		// A patch, because Fission's builder writes to a new package at
		// the same time and an update would conflict with it.
		base := wanted.Package.DeepCopy()
		wanted.Package.OwnerReferences = ownerOf(fn)
		if err := k8sClient.Patch(ctx, wanted.Package, client.MergeFrom(base)); err != nil {
			apierrors.Respond(c, apierrors.NewPartialFailure(fmt.Sprintf(
				"function %s and its code are saved, but the package did not get the function as its owner and the triggers were not applied; send the same body in a PUT to finish", fn.Name), err))
			return
		}
	}

	failed, conflicts, cause := applyTriggers(ctx, k8sClient, fn, existing, wanted)
	switch {
	case len(conflicts) > 0:
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf(
			"function %s is saved, but these trigger names are taken by objects of another function in the namespace: %s; rename them and send the body in a PUT",
			fn.Name, strings.Join(conflicts, ", "))))
		return
	case len(failed) > 0:
		apierrors.Respond(c, apierrors.NewPartialFailure(fmt.Sprintf(
			"function %s is saved, but these triggers were not applied: %s; send the same body in a PUT to finish",
			fn.Name, strings.Join(failed, ", ")), cause))
		return
	}

	c.JSON(status, FromK8s(wanted, h.routerURLs[c.Param("cluster")], h.settings))
}

// applyTriggers makes the triggers of fn match wanted: it creates the new
// ones, updates the changed ones and deletes those the request left out. It
// goes on after a failure, so one bad trigger does not hold back the others,
// and returns the API names of the triggers it could not apply.
func applyTriggers(ctx context.Context, k8sClient client.Client, fn *fissionv1.Function, existing, wanted *Objects) (failed, conflicts []string, cause error) {
	owner := ownerOf(fn)
	apiName := func(object string) string { return strings.TrimPrefix(object, fn.Name+"-") }
	record := func(object string, err error) {
		if err == nil {
			return
		}
		// The name is free of this function's triggers, so an object that
		// has it belongs to another function.
		if k8serrors.IsAlreadyExists(err) {
			conflicts = append(conflicts, apiName(object))
			return
		}
		failed = append(failed, apiName(object))
		cause = errors.Join(cause, err)
	}

	reconcileTriggers(ctx, k8sClient, owner, existing.HTTPTriggers, wanted.HTTPTriggers,
		func(trigger *fissionv1.HTTPTrigger) any { return trigger.Spec }, record)
	reconcileTriggers(ctx, k8sClient, owner, existing.TimeTriggers, wanted.TimeTriggers,
		func(trigger *fissionv1.TimeTrigger) any { return trigger.Spec }, record)

	sort.Strings(failed)
	sort.Strings(conflicts)
	return failed, conflicts, cause
}

// reconcileTriggers makes the stored triggers of one kind match wanted and
// reports the result of each write to record.
func reconcileTriggers[T any, P fissionObject[T]](ctx context.Context, k8sClient client.Client, owner []metav1.OwnerReference, existing, wanted []T, spec func(*T) any, record func(object string, err error)) {
	for i := range wanted {
		object := P(&wanted[i])
		object.SetOwnerReferences(owner)
		current := findByName[T, P](existing, object.GetName())
		switch {
		case current == nil:
			record(object.GetName(), k8sClient.Create(ctx, object))
		case !apiequality.Semantic.DeepEqual(spec(current), spec(&wanted[i])) ||
			!apiequality.Semantic.DeepEqual(P(current).GetOwnerReferences(), owner):
			record(object.GetName(), updateOverStatusWrites(ctx, k8sClient, object))
		}
	}
	for i := range existing {
		object := P(&existing[i])
		if findByName[T, P](wanted, object.GetName()) == nil {
			record(object.GetName(), client.IgnoreNotFound(k8sClient.Delete(ctx, object)))
		}
	}
}

// respondList groups the listed objects by function and writes a FunctionListResponse.
func (h *Handler) respondList(c *gin.Context, functions fissionv1.FunctionList, packages fissionv1.PackageList, httpTriggers fissionv1.HTTPTriggerList, timeTriggers fissionv1.TimeTriggerList) {
	type key struct{ namespace, name string }

	packageByKey := make(map[key]*fissionv1.Package, len(packages.Items))
	for i := range packages.Items {
		pkg := &packages.Items[i]
		packageByKey[key{pkg.Namespace, pkg.Name}] = pkg
	}
	httpByKey := make(map[key][]fissionv1.HTTPTrigger)
	for _, trigger := range httpTriggers.Items {
		if ref := trigger.Spec.FunctionReference; ref.Type == fissionv1.FunctionReferenceTypeFunctionName {
			k := key{trigger.Namespace, ref.Name}
			httpByKey[k] = append(httpByKey[k], trigger)
		}
	}
	timeByKey := make(map[key][]fissionv1.TimeTrigger)
	for _, trigger := range timeTriggers.Items {
		if trigger.Spec.Type == fissionv1.FunctionReferenceTypeFunctionName {
			k := key{trigger.Namespace, trigger.Spec.Name}
			timeByKey[k] = append(timeByKey[k], trigger)
		}
	}

	routerURL := h.routerURLs[c.Param("cluster")]
	items := make([]FunctionResponse, 0, len(functions.Items))
	for i := range functions.Items {
		fn := &functions.Items[i]
		k := key{fn.Namespace, fn.Name}
		items = append(items, FromK8s(&Objects{
			Function:     fn,
			Package:      packageByKey[key{fn.Namespace, fn.Spec.Package.PackageRef.Name}],
			HTTPTriggers: httpByKey[k],
			TimeTriggers: timeByKey[k],
		}, routerURL, h.settings))
	}
	c.JSON(http.StatusOK, FunctionListResponse{Items: items, Total: len(items)})
}
