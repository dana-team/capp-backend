package functions

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/cfuncsource"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/cfunc/pools"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultTimeoutSeconds               = 60
	defaultIdleTimeoutSeconds           = 120
	defaultSpecializationTimeoutSeconds = 120
	defaultRequestsPerPod               = 1
	maxRequestsPerPod                   = 1000
	maxEntrypointLength                 = 253

	// Fission mounts a Secret or ConfigMap of a function under these folders,
	// at <root>/<namespace>/<name>/<key>, and offers no other path.
	secretMountRoot    = "/secrets"
	configMapMountRoot = "/configs"

	// Fission has no field for where the code came from, so the package
	// records it. A read reports the source from these.
	SourceURLAnnotation    = "cfunc.dana.io/source-url"
	SourceRefAnnotation    = "cfunc.dana.io/source-ref"
	SourcePathAnnotation   = "cfunc.dana.io/source-path"
	SourceCommitAnnotation = "cfunc.dana.io/source-commit"

	// cronCheckRuns is how many consecutive runs of a schedule are compared
	// with the minimum interval.
	cronCheckRuns = 120
)

// cronParser accepts what Fission's timer accepts (IsValidCronSpec).
var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// cronCheckStart is a fixed instant, so the interval check gives the same
// answer for the same schedule at any time.
var cronCheckStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

var httpMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// Settings is what the conversion needs from the backend configuration.
type Settings struct {
	Functions        config.CfuncFunctionsConfig
	AllowCustomBuild bool
}

// Objects is the set of Fission objects behind one function.
type Objects struct {
	Function     *fissionv1.Function
	Package      *fissionv1.Package
	HTTPTriggers []fissionv1.HTTPTrigger
	TimeTriggers []fissionv1.TimeTrigger
}

// Validate checks everything in req that needs neither the cluster nor the
// git server, so that a bad request is refused before either is contacted.
func Validate(req FunctionRequest, namespace string, settings Settings) error {
	if msgs := validation.IsDNS1123Label(req.Name); len(msgs) > 0 {
		return fmt.Errorf("name %q: %s", req.Name, strings.Join(msgs, "; "))
	}
	if msgs := validation.IsDNS1123Label(req.Pool); len(msgs) > 0 {
		return fmt.Errorf("pool %q: %s", req.Pool, strings.Join(msgs, "; "))
	}
	if req.Source.URL == "" {
		return errors.New("source.url is required")
	}
	if _, err := cfuncsource.CleanPath(req.Source.Path); err != nil {
		return err
	}
	if len(req.Entrypoint) > maxEntrypointLength || strings.ContainsAny(req.Entrypoint, " \t\r\n") {
		return fmt.Errorf("entrypoint must be at most %d characters with no white space", maxEntrypointLength)
	}
	if req.TimeoutSeconds != nil {
		if err := inRange("timeoutSeconds", *req.TimeoutSeconds, 1, settings.Functions.MaxTimeoutSeconds); err != nil {
			return err
		}
	}
	if err := validateVolumes(req, namespace); err != nil {
		return err
	}
	if err := validateTriggers(req, namespace, settings); err != nil {
		return err
	}
	return validateAdvanced(req.Advanced, settings)
}

func inRange(field string, value, low, high int) error {
	if value < low || value > high {
		return fmt.Errorf("%s must be between %d and %d, got %d", field, low, high, value)
	}
	return nil
}

func validateVolumes(req FunctionRequest, namespace string) error {
	seen := make(map[string]struct{}, len(req.SecretVolumes))
	for i, volume := range req.SecretVolumes {
		field := fmt.Sprintf("secretVolumes[%d]", i)
		if err := validateVolume(field, "secretName", volume.SecretName, volume.Name, volume.MountPath,
			mountPath(secretMountRoot, namespace, volume.SecretName), seen); err != nil {
			return err
		}
	}
	seen = make(map[string]struct{}, len(req.ConfigMapVolumes))
	for i, volume := range req.ConfigMapVolumes {
		field := fmt.Sprintf("configMapVolumes[%d]", i)
		if err := validateVolume(field, "configMapName", volume.ConfigMapName, volume.Name, volume.MountPath,
			mountPath(configMapMountRoot, namespace, volume.ConfigMapName), seen); err != nil {
			return err
		}
	}
	return nil
}

// validateVolume checks one Secret or ConfigMap reference. Fission fixes the
// volume name and the mount path, so a request may leave them out or repeat
// what a response shows, and nothing else.
func validateVolume(field, key, resource, name, mount, wantMount string, seen map[string]struct{}) error {
	if resource == "" {
		return fmt.Errorf("%s.%s is required", field, key)
	}
	if msgs := validation.IsDNS1123Subdomain(resource); len(msgs) > 0 {
		return fmt.Errorf("%s.%s %q: %s", field, key, resource, strings.Join(msgs, "; "))
	}
	if _, duplicate := seen[resource]; duplicate {
		return fmt.Errorf("%s.%s %q is listed more than once", field, key, resource)
	}
	seen[resource] = struct{}{}
	if name != "" && name != resource {
		return fmt.Errorf("%s.name must be empty or %q: a function cannot name its volumes", field, resource)
	}
	if mount != "" && mount != wantMount {
		return fmt.Errorf("%s.mountPath must be empty or %q: Fission fixes the path of a function's files", field, wantMount)
	}
	return nil
}

func mountPath(root, namespace, name string) string {
	return root + "/" + namespace + "/" + name
}

func validateTriggers(req FunctionRequest, namespace string, settings Settings) error {
	if len(req.Triggers) > settings.Functions.MaxTriggers {
		return fmt.Errorf("a function can have at most %d triggers, got %d", settings.Functions.MaxTriggers, len(req.Triggers))
	}
	seen := make(map[string]struct{}, len(req.Triggers))
	routes := make(map[string]string, len(req.Triggers))
	for i, trigger := range req.Triggers {
		field := fmt.Sprintf("triggers[%d]", i)
		if msgs := validation.IsDNS1123Label(trigger.Name); len(msgs) > 0 {
			return fmt.Errorf("%s.name %q: %s", field, trigger.Name, strings.Join(msgs, "; "))
		}
		if _, duplicate := seen[trigger.Name]; duplicate {
			return fmt.Errorf("%s.name %q is used by more than one trigger", field, trigger.Name)
		}
		seen[trigger.Name] = struct{}{}
		if msgs := validation.IsDNS1123Label(triggerObjectName(req.Name, trigger.Name)); len(msgs) > 0 {
			return fmt.Errorf("%s.name %q: the function name and the trigger name are together longer than %d characters",
				field, trigger.Name, validation.DNS1123LabelMaxLength-1)
		}

		var err error
		switch trigger.Type {
		case TriggerTypeHTTP:
			err = validateHTTPTrigger(trigger, namespace, req.Name)
			if err == nil {
				err = claimRoutes(trigger, routes)
			}
		case TriggerTypeCron:
			err = validateCronTrigger(trigger, settings.Functions.MinCronIntervalSeconds)
		default:
			err = fmt.Errorf("type must be %q or %q, got %q", TriggerTypeHTTP, TriggerTypeCron, trigger.Type)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}

// claimRoutes records the path and methods of an http trigger and fails
// when another trigger of the request has one of them already. Fission
// admits both and then reports RouteConflict on one, so it is refused here.
func claimRoutes(trigger FunctionTrigger, routes map[string]string) error {
	methods := trigger.Methods
	if len(methods) == 0 {
		methods = []string{http.MethodGet}
	}
	path := strings.TrimSuffix(trigger.Path, "/")
	for _, method := range methods {
		key := method + " " + path
		if other, taken := routes[key]; taken && other != trigger.Name {
			return fmt.Errorf("%s on path %q is already served by trigger %q", method, trigger.Path, other)
		}
		routes[key] = trigger.Name
	}
	return nil
}

func validateHTTPTrigger(trigger FunctionTrigger, namespace, function string) error {
	if trigger.Schedule != "" {
		return errors.New("schedule belongs to a cron trigger")
	}
	if trigger.Path != "" && !strings.HasPrefix(trigger.Path, "/") {
		return fmt.Errorf("path %q must start with a slash", trigger.Path)
	}
	if strings.Contains(trigger.Path, "//") || strings.ContainsAny(trigger.Path, " \t\r\n?#") {
		return fmt.Errorf("path %q must be a URL path with no empty segment, query or white space", trigger.Path)
	}
	for _, method := range trigger.Methods {
		if !slices.Contains(httpMethods, method) {
			return fmt.Errorf("methods: %q is not one of %s", method, strings.Join(httpMethods, ", "))
		}
	}
	spec := fissionv1.HTTPTriggerSpec{
		RelativeURL:       fullPath(namespace, function, trigger.Path),
		FunctionReference: functionReference(function),
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("path %q: %w", trigger.Path, err)
	}
	return nil
}

func validateCronTrigger(trigger FunctionTrigger, minIntervalSeconds int) error {
	if trigger.Path != "" || len(trigger.Methods) > 0 {
		return errors.New("path and methods belong to an http trigger")
	}
	if trigger.Schedule == "" {
		return errors.New("schedule is required")
	}
	schedule, err := cronParser.Parse(trigger.Schedule)
	if err != nil {
		return fmt.Errorf("schedule %q: %w", trigger.Schedule, err)
	}
	minInterval := time.Duration(minIntervalSeconds) * time.Second
	previous := schedule.Next(cronCheckStart)
	for range cronCheckRuns {
		next := schedule.Next(previous)
		if next.IsZero() {
			break
		}
		if next.Sub(previous) < minInterval {
			return fmt.Errorf("schedule %q runs more often than one time in %d seconds", trigger.Schedule, minIntervalSeconds)
		}
		previous = next
	}
	return nil
}

func validateAdvanced(advanced *FunctionAdvanced, settings Settings) error {
	if advanced == nil {
		return nil
	}
	concurrency := settings.Functions.DefaultConcurrency
	if advanced.Concurrency != nil {
		concurrency = *advanced.Concurrency
		if err := inRange("advanced.concurrency", concurrency, 1, settings.Functions.MaxConcurrency); err != nil {
			return err
		}
	}
	if advanced.RequestsPerPod != nil {
		if err := inRange("advanced.requestsPerPod", *advanced.RequestsPerPod, 1, maxRequestsPerPod); err != nil {
			return err
		}
	}
	if advanced.IdleTimeoutSeconds != nil && *advanced.IdleTimeoutSeconds < 1 {
		return fmt.Errorf("advanced.idleTimeoutSeconds must be 1 or greater, got %d", *advanced.IdleTimeoutSeconds)
	}
	if advanced.RetainPods != nil {
		if err := inRange("advanced.retainPods", *advanced.RetainPods, 0, concurrency); err != nil {
			return err
		}
	}
	if advanced.SpecializationTimeoutSeconds != nil && *advanced.SpecializationTimeoutSeconds < defaultSpecializationTimeoutSeconds {
		return fmt.Errorf("advanced.specializationTimeoutSeconds must be %d or greater, got %d",
			defaultSpecializationTimeoutSeconds, *advanced.SpecializationTimeoutSeconds)
	}
	if advanced.BuildCommand != "" && !settings.AllowCustomBuild {
		return errors.New("advanced.buildCommand is not allowed: the backend does not enable custom builds (resources.cfunc.allowCustomBuild)")
	}
	return nil
}

// ToK8s converts a FunctionRequest into the Fission objects behind it.
// existing is nil on create and the live set on update; its objects are
// copied first so that fields this API does not expose survive. pool is the
// function's pool and code is the result of the git fetch.
//
// The result leaves three things to the caller, because they need objects
// that exist: the package reference's resource version, the owner references
// and the deletion of triggers that are no longer wanted.
func ToK8s(req FunctionRequest, existing *Objects, namespace string, pool *fissionv1.Environment, code *cfuncsource.Archive, settings Settings) (*Objects, error) {
	if err := Validate(req, namespace, settings); err != nil {
		return nil, err
	}
	if existing == nil {
		existing = &Objects{}
	}
	hasBuilder := pool.Spec.Builder.Image != ""
	build := hasBuilder && !code.IsFile
	if req.Source.Build != nil {
		build = *req.Source.Build
	}
	if build && !hasBuilder {
		return nil, fmt.Errorf("source.build needs a pool that builds code, and pool %s has no build image", pool.Name)
	}
	if req.Advanced != nil && req.Advanced.BuildCommand != "" && !build {
		return nil, fmt.Errorf("advanced.buildCommand needs code that is built: a pool with a build image (pool %s) and source.build not false", pool.Name)
	}

	objects := &Objects{
		Function: functionToK8s(req, existing.Function, namespace, settings),
		Package:  packageToK8s(req, existing.Package, namespace, build, code),
	}
	// The limit applies to the bytes that are stored, which are the zip for
	// built code and the file itself for one file that is not built.
	stored := len(objects.Package.Spec.Source.Literal)
	if !build {
		stored = len(objects.Package.Spec.Deployment.Literal)
	}
	if int64(stored) > fissionv1.ArchiveLiteralSizeLimit {
		return nil, fmt.Errorf("the code in source.path %q is %d bytes as it is stored; the limit is %d bytes",
			req.Source.Path, stored, fissionv1.ArchiveLiteralSizeLimit)
	}
	for _, trigger := range req.Triggers {
		name := triggerObjectName(req.Name, trigger.Name)
		if trigger.Type == TriggerTypeHTTP {
			objects.HTTPTriggers = append(objects.HTTPTriggers,
				httpTriggerToK8s(trigger, findByName(existing.HTTPTriggers, name), name, namespace, req.Name))
			continue
		}
		objects.TimeTriggers = append(objects.TimeTriggers,
			timeTriggerToK8s(trigger, findByName(existing.TimeTriggers, name), name, namespace, req.Name))
	}
	return objects, nil
}

func functionToK8s(req FunctionRequest, existing *fissionv1.Function, namespace string, settings Settings) *fissionv1.Function {
	fn := &fissionv1.Function{
		TypeMeta:   metav1.TypeMeta{APIVersion: fissionv1.SchemeGroupVersion.String(), Kind: "Function"},
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: namespace},
	}
	if existing != nil {
		fn = existing.DeepCopy()
	}

	advanced := FunctionAdvanced{}
	if req.Advanced != nil {
		advanced = *req.Advanced
	}

	fn.Spec.Environment = fissionv1.EnvironmentReference{Namespace: namespace, Name: req.Pool}
	fn.Spec.Package.PackageRef.Namespace = namespace
	fn.Spec.Package.PackageRef.Name = req.Name
	fn.Spec.Package.FunctionName = req.Entrypoint

	fn.Spec.Secrets = nil
	for _, volume := range req.SecretVolumes {
		fn.Spec.Secrets = append(fn.Spec.Secrets, fissionv1.SecretReference{Namespace: namespace, Name: volume.SecretName})
	}
	fn.Spec.ConfigMaps = nil
	for _, volume := range req.ConfigMapVolumes {
		fn.Spec.ConfigMaps = append(fn.Spec.ConfigMaps, fissionv1.ConfigMapReference{Namespace: namespace, Name: volume.ConfigMapName})
	}

	fn.Spec.InvokeStrategy.StrategyType = fissionv1.StrategyTypeExecution
	fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType = fissionv1.ExecutorTypePoolmgr
	fn.Spec.InvokeStrategy.ExecutionStrategy.SpecializationTimeout =
		valueOr(advanced.SpecializationTimeoutSeconds, defaultSpecializationTimeoutSeconds)

	fn.Spec.FunctionTimeout = valueOr(req.TimeoutSeconds, defaultTimeoutSeconds)
	idle := valueOr(advanced.IdleTimeoutSeconds, defaultIdleTimeoutSeconds)
	fn.Spec.IdleTimeout = &idle
	fn.Spec.Concurrency = valueOr(advanced.Concurrency, settings.Functions.DefaultConcurrency)
	fn.Spec.RequestsPerPod = valueOr(advanced.RequestsPerPod, defaultRequestsPerPod)
	fn.Spec.OnceOnly = advanced.OnceOnly
	fn.Spec.RetainPods = valueOr(advanced.RetainPods, 0)
	return fn
}

func valueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// packageToK8s puts the code where Fission needs it: code that is built goes
// in as a source archive for the builder, and any other code goes in as the
// deployment archive the runtime loads as it is.
func packageToK8s(req FunctionRequest, existing *fissionv1.Package, namespace string, build bool, code *cfuncsource.Archive) *fissionv1.Package {
	pkg := &fissionv1.Package{
		TypeMeta:   metav1.TypeMeta{APIVersion: fissionv1.SchemeGroupVersion.String(), Kind: "Package"},
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: namespace},
	}
	if existing != nil {
		pkg = existing.DeepCopy()
	}

	pkg.Spec.Environment = fissionv1.EnvironmentReference{Namespace: namespace, Name: req.Pool}
	pkg.Spec.BuildCommand = ""
	if req.Advanced != nil {
		pkg.Spec.BuildCommand = req.Advanced.BuildCommand
	}

	if build {
		// The deployment archive of a built package is the builder's output.
		// It is the builder's to replace, so it is left as it is.
		if existing == nil || existing.Spec.Source.IsEmpty() {
			pkg.Spec.Deployment = fissionv1.Archive{}
		}
		pkg.Spec.Source = literalArchive(code.Zip)
	} else {
		pkg.Spec.Source = fissionv1.Archive{}
		literal := code.Zip
		if code.SingleFile != nil {
			literal = code.SingleFile
		}
		pkg.Spec.Deployment = literalArchive(literal)
	}

	cleanPath, _ := cfuncsource.CleanPath(req.Source.Path)
	if pkg.Annotations == nil {
		pkg.Annotations = map[string]string{}
	}
	pkg.Annotations[SourceURLAnnotation] = req.Source.URL
	pkg.Annotations[SourceRefAnnotation] = req.Source.Ref
	pkg.Annotations[SourcePathAnnotation] = cleanPath
	pkg.Annotations[SourceCommitAnnotation] = code.Commit
	return pkg
}

func literalArchive(content []byte) fissionv1.Archive {
	return fissionv1.Archive{Type: fissionv1.ArchiveTypeLiteral, Literal: content}
}

func triggerObjectName(function, trigger string) string {
	return function + "-" + trigger
}

// fullPath is the path the router serves. The namespace and the function
// name come first because the router has one URL space for the whole
// cluster, so two namespaces could otherwise claim the same path.
func fullPath(namespace, function, path string) string {
	return "/" + namespace + "/" + function + strings.TrimSuffix(path, "/")
}

func functionReference(function string) fissionv1.FunctionReference {
	return fissionv1.FunctionReference{Type: fissionv1.FunctionReferenceTypeFunctionName, Name: function}
}

func httpTriggerToK8s(trigger FunctionTrigger, existing *fissionv1.HTTPTrigger, name, namespace, function string) fissionv1.HTTPTrigger {
	object := fissionv1.HTTPTrigger{
		TypeMeta:   metav1.TypeMeta{APIVersion: fissionv1.SchemeGroupVersion.String(), Kind: "HTTPTrigger"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if existing != nil {
		object = *existing.DeepCopy()
	}
	object.Spec.RelativeURL = fullPath(namespace, function, trigger.Path)
	object.Spec.Prefix = nil
	object.Spec.Method = ""
	object.Spec.Methods = slices.Clone(trigger.Methods)
	if len(object.Spec.Methods) == 0 {
		object.Spec.Methods = []string{http.MethodGet}
	}
	object.Spec.FunctionReference = functionReference(function)
	return object
}

func timeTriggerToK8s(trigger FunctionTrigger, existing *fissionv1.TimeTrigger, name, namespace, function string) fissionv1.TimeTrigger {
	object := fissionv1.TimeTrigger{
		TypeMeta:   metav1.TypeMeta{APIVersion: fissionv1.SchemeGroupVersion.String(), Kind: "TimeTrigger"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if existing != nil {
		object = *existing.DeepCopy()
	}
	object.Spec.Cron = trigger.Schedule
	object.Spec.FunctionReference = functionReference(function)
	return object
}

// fissionObject is a pointer to a Fission object type T. It lets one helper
// serve HTTPTriggers and TimeTriggers, which share no interface but this.
type fissionObject[T any] interface {
	*T
	client.Object
}

func findByName[T any, P fissionObject[T]](items []T, name string) *T {
	for i := range items {
		if P(&items[i]).GetName() == name {
			return &items[i]
		}
	}
	return nil
}

func httpTriggerRef(trigger *fissionv1.HTTPTrigger) fissionv1.FunctionReference {
	return trigger.Spec.FunctionReference
}

func timeTriggerRef(trigger *fissionv1.TimeTrigger) fissionv1.FunctionReference {
	return trigger.Spec.FunctionReference
}

// triggersOf returns the triggers that belong to function: those whose
// reference names it. Fission resolves that name in the trigger's own
// namespace.
func triggersOf[T any](triggers []T, function string, ref func(*T) fissionv1.FunctionReference) []T {
	var owned []T
	for i := range triggers {
		if r := ref(&triggers[i]); r.Type == fissionv1.FunctionReferenceTypeFunctionName && r.Name == function {
			owned = append(owned, triggers[i])
		}
	}
	return owned
}

// FromK8s converts the Fission objects of one function into a
// FunctionResponse. objects.Package may be nil when the package is missing.
// routerURL is the base URL of the cluster's Fission router, or empty.
func FromK8s(objects *Objects, routerURL string, settings Settings) FunctionResponse {
	fn := objects.Function
	resp := FunctionResponse{
		Name:            fn.Name,
		Namespace:       fn.Namespace,
		UID:             string(fn.UID),
		ResourceVersion: fn.ResourceVersion,
		Pool:            fn.Spec.Environment.Name,
		Entrypoint:      fn.Spec.Package.FunctionName,
		TimeoutSeconds:  fn.Spec.FunctionTimeout,
		Triggers:        triggersFromK8s(objects, routerURL),
		Advanced:        advancedFromK8s(fn, objects.Package, settings),
		Status: FunctionStatusResponse{
			ObservedGeneration: fn.Status.ObservedGeneration,
			Conditions:         pools.ConditionsFromK8s(fn.Status.Conditions),
		},
	}
	if !fn.CreationTimestamp.IsZero() {
		resp.CreatedAt = fn.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	if resp.TimeoutSeconds == 0 {
		resp.TimeoutSeconds = defaultTimeoutSeconds
	}
	for _, secret := range fn.Spec.Secrets {
		resp.SecretVolumes = append(resp.SecretVolumes, SecretVolume{
			Name:       secret.Name,
			SecretName: secret.Name,
			MountPath:  mountPath(secretMountRoot, fn.Namespace, secret.Name),
		})
	}
	for _, configMap := range fn.Spec.ConfigMaps {
		resp.ConfigMapVolumes = append(resp.ConfigMapVolumes, ConfigMapVolume{
			Name:          configMap.Name,
			ConfigMapName: configMap.Name,
			MountPath:     mountPath(configMapMountRoot, fn.Namespace, configMap.Name),
		})
	}
	if pkg := objects.Package; pkg != nil {
		resp.Source = FunctionSource{
			URL:    pkg.Annotations[SourceURLAnnotation],
			Ref:    pkg.Annotations[SourceRefAnnotation],
			Path:   pkg.Annotations[SourcePathAnnotation],
			Commit: pkg.Annotations[SourceCommitAnnotation],
		}
		built := !pkg.Spec.Source.IsEmpty()
		resp.Source.Build = &built
		resp.Build = FunctionBuild{Status: string(pkg.Status.BuildStatus), Log: pkg.Status.BuildLog}
	}
	return resp
}

// advancedFromK8s returns the values that are not the defaults, or nil when
// every one is. A response sent back in a PUT then keeps them.
func advancedFromK8s(fn *fissionv1.Function, pkg *fissionv1.Package, settings Settings) *FunctionAdvanced {
	advanced := FunctionAdvanced{OnceOnly: fn.Spec.OnceOnly}
	set := fn.Spec.OnceOnly

	keep := func(target **int, value, fallback int) {
		if value != 0 && value != fallback {
			*target = &value
			set = true
		}
	}
	keep(&advanced.Concurrency, fn.Spec.Concurrency, settings.Functions.DefaultConcurrency)
	keep(&advanced.RequestsPerPod, fn.Spec.RequestsPerPod, defaultRequestsPerPod)
	keep(&advanced.RetainPods, fn.Spec.RetainPods, 0)
	keep(&advanced.SpecializationTimeoutSeconds,
		fn.Spec.InvokeStrategy.ExecutionStrategy.SpecializationTimeout, defaultSpecializationTimeoutSeconds)
	if fn.Spec.IdleTimeout != nil {
		keep(&advanced.IdleTimeoutSeconds, *fn.Spec.IdleTimeout, defaultIdleTimeoutSeconds)
	}
	if pkg != nil && pkg.Spec.BuildCommand != "" {
		advanced.BuildCommand = pkg.Spec.BuildCommand
		set = true
	}

	if !set {
		return nil
	}
	return &advanced
}

func triggersFromK8s(objects *Objects, routerURL string) []FunctionTrigger {
	fn := objects.Function
	prefix := fullPath(fn.Namespace, fn.Name, "")
	triggers := make([]FunctionTrigger, 0, len(objects.HTTPTriggers)+len(objects.TimeTriggers))
	// managed[i] is true when triggers[i] has the object name this API gives
	// it. A name is managed for one kind only: an HTTPTrigger and a
	// TimeTrigger of the same object name cannot both keep it.
	var managed []bool
	seenManaged := map[string]struct{}{}
	apiName := func(object string) string {
		name, hasPrefix := strings.CutPrefix(object, fn.Name+"-")
		_, taken := seenManaged[name]
		managed = append(managed, hasPrefix && !taken)
		if hasPrefix {
			seenManaged[name] = struct{}{}
		}
		return name
	}

	for i := range objects.HTTPTriggers {
		object := &objects.HTTPTriggers[i]
		trigger := FunctionTrigger{
			Name:     apiName(object.Name),
			Type:     TriggerTypeHTTP,
			Path:     object.Spec.RelativeURL,
			Methods:  slices.Clone(object.Spec.Methods),
			FullPath: object.Spec.RelativeURL,
		}
		// A trigger made outside this API may have no prefix. Its path then
		// reads as stored, and the next PUT moves it under the prefix.
		if rest, ok := strings.CutPrefix(object.Spec.RelativeURL, prefix); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			trigger.Path = rest
		}
		if len(trigger.Methods) == 0 && object.Spec.Method != "" {
			trigger.Methods = []string{object.Spec.Method}
		}
		if routerURL != "" {
			trigger.URL = strings.TrimSuffix(routerURL, "/") + trigger.FullPath
		}
		trigger.Status, trigger.Message = readyReason(object.Status.Conditions)
		triggers = append(triggers, trigger)
	}

	for i := range objects.TimeTriggers {
		object := &objects.TimeTriggers[i]
		trigger := FunctionTrigger{
			Name:     apiName(object.Name),
			Type:     TriggerTypeCron,
			Schedule: object.Spec.Cron,
		}
		trigger.Status, trigger.Message = readyReason(object.Status.Conditions)
		triggers = append(triggers, trigger)
	}

	uniqueTriggerNames(triggers, managed)
	sort.SliceStable(triggers, func(i, j int) bool { return triggers[i].Name < triggers[j].Name })
	return triggers
}

// uniqueTriggerNames makes every API name in triggers different. Two objects
// can map to one name: a trigger named "x" that was made outside this API
// and the trigger "<function>-x" that this API made, or an HTTPTrigger and a
// TimeTrigger of the same object name. A response with two triggers of one
// name could not be sent back in a PUT, and the test route could not select
// one. The triggers this API named keep their names; another one gets a
// number, and the next PUT stores it under that name.
func uniqueTriggerNames(triggers []FunctionTrigger, managed []bool) {
	taken := make(map[string]struct{}, len(triggers))
	for i := range triggers {
		if managed[i] {
			taken[triggers[i].Name] = struct{}{}
		}
	}
	for i := range triggers {
		if managed[i] {
			continue
		}
		name := triggers[i].Name
		for n := 2; ; n++ {
			if _, used := taken[name]; !used {
				break
			}
			name = fmt.Sprintf("%s-%d", triggers[i].Name, n)
		}
		triggers[i].Name = name
		taken[name] = struct{}{}
	}
}

// readyReason returns the reason of the Ready condition, and its message
// when the object is not ready.
func readyReason(conditions []metav1.Condition) (reason, message string) {
	for _, condition := range conditions {
		if condition.Type != "Ready" {
			continue
		}
		if condition.Status != metav1.ConditionTrue {
			return condition.Reason, condition.Message
		}
		return condition.Reason, ""
	}
	return "", ""
}
