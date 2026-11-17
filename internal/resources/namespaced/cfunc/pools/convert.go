package pools

import (
	"fmt"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/containerspec"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// EnvironmentVersion is the interface version every pool is written
	// with. Fission honours spec.poolsize only for version 3 Environments.
	EnvironmentVersion = 3

	// FissionEnabledLabel on a Namespace makes Fission serve the namespace
	// when it runs with tenancy.mode=dynamic.
	FissionEnabledLabel = "fission.io/enabled"
	FissionEnabledValue = "true"

	defaultPoolSize = 3

	// Fission v1.27.0 stores an omitted terminationGracePeriod as 0 and then
	// kills pods with no drain window, so the value is always written. 360 is
	// what the Fission docs and CLI use.
	defaultTerminationGracePeriodSeconds int64 = 360

	// With no command the Fission builder runs "/build", which the official
	// build images do not have; their script is "build" on the PATH. The
	// Fission CLI writes the same default.
	defaultBuildCommand = "build"
)

// ToK8s converts a PoolRequest into a fission.io/v1 Environment
// resource suitable for creating or replacing via the Kubernetes API.
//
// The request names a flavor and a size; the images, build settings and
// resources behind them come from the catalog. The advanced block, when
// present, overrides the build image, the build command and the resources.
// resourceVersion is NOT set here; the update handler reads it from the
// live object and sets it before calling Update.
func ToK8s(req PoolRequest, existing *fissionv1.Environment, namespace string, catalog *Catalog) (*fissionv1.Environment, error) {
	env := &fissionv1.Environment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "fission.io/v1",
			Kind:       "Environment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: fissionv1.EnvironmentSpec{
			Version: EnvironmentVersion,
		},
	}

	// Copy unmanaged fields from the existing object to preserve them across
	// updates: pod specs and the immutable version.
	if existing != nil {
		env.ObjectMeta = *existing.ObjectMeta.DeepCopy()
		env.Name = req.Name
		env.Namespace = namespace
		env.Spec = *existing.Spec.DeepCopy()
	}

	// 0 is also what an Environment created outside this API holds when the
	// field was left out, so it is treated as unset on update too.
	if env.Spec.TerminationGracePeriod == 0 {
		env.Spec.TerminationGracePeriod = defaultTerminationGracePeriodSeconds
	}

	flavor, ok := catalog.cfg.Flavors[req.Flavor]
	if !ok {
		return nil, fmt.Errorf("unknown flavor %q; available flavors: %s", req.Flavor, strings.Join(catalog.flavorNames, ", "))
	}

	poolSize := defaultPoolSize
	if req.PoolSize != nil {
		poolSize = *req.PoolSize
	}
	if poolSize < 0 {
		return nil, fmt.Errorf("poolSize must be 0 or greater, got %d", poolSize)
	}
	if poolSize > catalog.cfg.MaxPoolSize {
		return nil, fmt.Errorf("poolSize %d is above the maximum of %d", poolSize, catalog.cfg.MaxPoolSize)
	}

	resources, err := resourcesToK8s(req, catalog)
	if err != nil {
		return nil, err
	}

	runtimeEnv, err := containerspec.EnvVarsToK8s(req.Env)
	if err != nil {
		return nil, err
	}

	buildImage, buildCommand, err := buildSettingsToK8s(req, flavor, catalog.cfg)
	if err != nil {
		return nil, err
	}

	env.Spec.Runtime.Image = catalog.cfg.ImageRef(flavor.Image)
	setRuntimeEnv(env, runtimeEnv)
	env.Spec.Builder.Image = buildImage
	env.Spec.Builder.Command = buildCommand
	setBuilderEnv(env, flavor.BuilderEnv)
	env.Spec.KeepArchive = flavor.KeepArchive
	env.Spec.ImagePullSecret = catalog.cfg.ImagePullSecret
	env.Spec.Poolsize = poolSize
	env.Spec.Resources = resources

	return env, nil
}

// buildSettingsToK8s returns the build image and command of the flavor, or
// the advanced overrides when the configuration allows them.
func buildSettingsToK8s(req PoolRequest, flavor config.CfuncFlavor, cfg config.CfuncConfig) (image, command string, err error) {
	image, command = flavorBuildSettings(flavor, cfg)

	customImage, customCommand := "", ""
	if req.Advanced != nil {
		customImage, customCommand = req.Advanced.BuildImage, req.Advanced.BuildCommand
	}
	// A custom image or command makes the Fission builder run whatever the
	// caller names, so it is off unless the operator turns it on.
	if (customImage != "" || customCommand != "") && !cfg.AllowCustomBuild {
		return "", "", fmt.Errorf("advanced.buildImage and advanced.buildCommand are not enabled on this backend; pools build with the settings of their flavor")
	}
	if customImage != "" {
		if _, err := name.ParseReference(customImage); err != nil {
			return "", "", fmt.Errorf("advanced.buildImage %q is not a valid image reference", customImage)
		}
		image = customImage
	}
	if customCommand != "" {
		command = customCommand
	}

	if command != "" && image == "" {
		return "", "", fmt.Errorf("a build command needs a build image, and flavor %q has none; set advanced.buildImage", req.Flavor)
	}
	if image != "" && command == "" {
		command = defaultBuildCommand
	}
	return image, command, nil
}

// flavorBuildSettings returns the build image and command a pool of the
// flavor gets when the request overrides neither.
func flavorBuildSettings(flavor config.CfuncFlavor, cfg config.CfuncConfig) (image, command string) {
	image = cfg.ImageRef(flavor.BuildImage)
	command = flavor.BuildCommand
	if image != "" && command == "" {
		command = defaultBuildCommand
	}
	return image, command
}

// resourcesToK8s returns the resources of the requested or default size,
// with the advanced resources laid over them.
//
// A value the advanced block leaves out comes from the size. Without that, a
// block with a single request would produce a pod with no limits at all,
// which is more than the largest size allows.
func resourcesToK8s(req PoolRequest, catalog *Catalog) (corev1.ResourceRequirements, error) {
	sizeName := req.Size
	if sizeName == "" {
		sizeName = catalog.cfg.DefaultSize
	}
	size, ok := catalog.sizes[sizeName]
	if !ok {
		return corev1.ResourceRequirements{}, fmt.Errorf("unknown size %q; available sizes: %s", sizeName, strings.Join(catalog.sizeNames, ", "))
	}
	resources := *size.DeepCopy()

	if req.Advanced == nil || req.Advanced.Resources == nil {
		return resources, nil
	}
	custom, err := containerspec.ParseCustomResources(req.Advanced.Resources)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("advanced.resources: %w", err)
	}
	if len(custom.Requests) == 0 && len(custom.Limits) == 0 {
		return resources, nil
	}
	for resourceName, quantity := range custom.Requests {
		resources.Requests[resourceName] = quantity
	}
	for resourceName, quantity := range custom.Limits {
		resources.Limits[resourceName] = quantity
	}
	if err := validateCustomResources(resources, catalog.ceiling); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	return resources, nil
}

// validateCustomResources rejects negative quantities, a request above its
// limit, and any value above the largest configured size.
func validateCustomResources(res corev1.ResourceRequirements, ceiling corev1.ResourceList) error {
	for _, list := range []struct {
		kind   string
		values corev1.ResourceList
	}{{"requests", res.Requests}, {"limits", res.Limits}} {
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			quantity, ok := list.values[resourceName]
			if !ok {
				continue
			}
			if quantity.Sign() < 0 {
				return fmt.Errorf("advanced.resources.%s.%s must not be negative", list.kind, resourceName)
			}
			if limit, ok := ceiling[resourceName]; ok && quantity.Cmp(limit) > 0 {
				return fmt.Errorf("advanced.resources.%s.%s %s is above the largest configured size (%s)", list.kind, resourceName, quantity.String(), limit.String())
			}
		}
	}
	for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := res.Requests[resourceName], res.Limits[resourceName]
		if request.Cmp(limit) > 0 {
			return fmt.Errorf("advanced.resources.requests.%s %s is larger than its limit %s", resourceName, request.String(), limit.String())
		}
	}
	return nil
}

// setRuntimeEnv replaces the env list of the runtime container and keeps the
// rest of a container that was customised outside this API. Fission names
// the runtime container after the Environment.
func setRuntimeEnv(env *fissionv1.Environment, variables []corev1.EnvVar) {
	container := env.Spec.Runtime.Container

	var existing []corev1.EnvVar
	if container != nil {
		existing = container.Env
	}
	merged := containerspec.MergeEnvVars(variables, existing)

	if len(merged) == 0 {
		if container != nil {
			container.Env = nil
		}
		return
	}
	if container == nil {
		container = &corev1.Container{Name: env.Name}
		env.Spec.Runtime.Container = container
	}
	container.Env = merged
}

// setBuilderEnv replaces the env list of the builder container and keeps the
// rest of a container that was customised outside this API.
func setBuilderEnv(env *fissionv1.Environment, variables []config.CfuncEnvVar) {
	if env.Spec.Builder.Image == "" || len(variables) == 0 {
		if env.Spec.Builder.Container != nil {
			env.Spec.Builder.Container.Env = nil
		}
		return
	}
	if env.Spec.Builder.Container == nil {
		env.Spec.Builder.Container = &corev1.Container{Name: fissionv1.BuilderContainerName}
	}
	list := make([]corev1.EnvVar, len(variables))
	for i, variable := range variables {
		list[i] = corev1.EnvVar{Name: variable.Name, Value: variable.Value}
	}
	env.Spec.Builder.Container.Env = list
}

// sizeFromResources returns the configured size whose resources equal res.
// Returns empty string when no size matches (e.g. advanced resources).
func sizeFromResources(res corev1.ResourceRequirements, catalog *Catalog) string {
	for _, sizeName := range catalog.sizeNames {
		size := catalog.sizes[sizeName]
		if apiequality.Semantic.DeepEqual(size.Requests, res.Requests) && apiequality.Semantic.DeepEqual(size.Limits, res.Limits) {
			return sizeName
		}
	}
	return ""
}

// FromK8s converts a live fission.io/v1 Environment into a PoolResponse DTO.
//
// Nothing about the flavor or size is stored on the Environment: both are
// reported by matching the runtime image and the resources against the
// catalog.
func FromK8s(env *fissionv1.Environment, catalog *Catalog) PoolResponse {
	resp := PoolResponse{
		Name:            env.Name,
		Namespace:       env.Namespace,
		UID:             string(env.UID),
		ResourceVersion: env.ResourceVersion,
		Labels:          env.Labels,
		Annotations:     env.Annotations,

		Flavor:    catalog.flavorByImage[env.Spec.Runtime.Image],
		Size:      sizeFromResources(env.Spec.Resources, catalog),
		PoolSize:  env.Spec.Poolsize,
		Resources: containerspec.ResourceSpecFromK8s(env.Spec.Resources),
		Image:     env.Spec.Runtime.Image,

		Version:                       env.Spec.Version,
		TerminationGracePeriodSeconds: env.Spec.TerminationGracePeriod,
		AllowedFunctionsPerContainer:  string(env.Spec.AllowedFunctionsPerContainer),
		AllowAccessToExternalNetwork:  env.Spec.AllowAccessToExternalNetwork,

		Status: buildStatus(env),
	}
	if !env.CreationTimestamp.IsZero() {
		resp.CreatedAt = env.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	if env.Spec.Runtime.Container != nil {
		if variables := containerspec.EnvVarsFromK8s(env.Spec.Runtime.Container.Env); len(variables) > 0 {
			resp.Env = variables
		}
	}
	if env.Spec.Builder.Image != "" {
		resp.Builder = &Builder{Image: env.Spec.Builder.Image, Command: env.Spec.Builder.Command}
	}
	resp.Advanced = advancedFromK8s(env, resp, catalog)
	return resp
}

// advancedFromK8s returns the advanced block that reproduces what the
// Environment holds beyond its flavor and size, or nil when there is nothing.
//
// PUT is a full replace, so a client that sends a response back must carry
// the custom resources and build settings in a field the request has;
// otherwise they would silently fall back to the defaults.
func advancedFromK8s(env *fissionv1.Environment, resp PoolResponse, catalog *Catalog) *Advanced {
	advanced := Advanced{}
	if resp.Size == "" {
		advanced.Resources = resp.Resources
	}
	if flavor, ok := catalog.cfg.Flavors[resp.Flavor]; ok && env.Spec.Builder.Image != "" {
		image, command := flavorBuildSettings(flavor, catalog.cfg)
		if env.Spec.Builder.Image != image {
			advanced.BuildImage = env.Spec.Builder.Image
		}
		if env.Spec.Builder.Command != command {
			advanced.BuildCommand = env.Spec.Builder.Command
		}
	}
	if advanced == (Advanced{}) {
		return nil
	}
	return &advanced
}

// buildStatus maps the Environment's Status to the simplified PoolStatusResponse DTO.
func buildStatus(env *fissionv1.Environment) PoolStatusResponse {
	return PoolStatusResponse{
		ObservedGeneration: env.Status.ObservedGeneration,
		Conditions:         ConditionsFromK8s(env.Status.Conditions),
	}
}

// ConditionsFromK8s converts the status conditions of a Fission object. It
// never returns nil, so a response always carries a list.
func ConditionsFromK8s(in []metav1.Condition) []Condition {
	conditions := make([]Condition, 0, len(in))
	for _, condition := range in {
		c := Condition{
			Type:    condition.Type,
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
		}
		if !condition.LastTransitionTime.IsZero() {
			c.LastTransitionTime = condition.LastTransitionTime.UTC().Format(time.RFC3339)
		}
		conditions = append(conditions, c)
	}
	return conditions
}
