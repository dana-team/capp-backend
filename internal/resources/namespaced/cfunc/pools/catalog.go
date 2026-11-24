package pools

import (
	"fmt"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/containerspec"
	corev1 "k8s.io/api/core/v1"
)

// Catalog is the cfunc configuration resolved once at startup: the parsed
// resources of every size, the largest limit of any size, and the flavor of
// every runtime image. The configuration does not change while the server
// runs, so nothing here is recomputed per request or per listed pool.
type Catalog struct {
	cfg config.CfuncConfig

	flavorNames      []string
	sizeNames        []string
	flavorByImage    map[string]string
	sizes            map[string]corev1.ResourceRequirements
	ceiling          corev1.ResourceList
	fetcherResources corev1.ResourceRequirements
	builderResources corev1.ResourceRequirements
}

// NewCatalog resolves cfg. It fails on a size that does not parse, which
// config.Validate already rejects for an enabled cfunc.
func NewCatalog(cfg config.CfuncConfig) (*Catalog, error) {
	c := &Catalog{
		cfg:           cfg,
		flavorNames:   cfg.FlavorNames(),
		sizeNames:     cfg.SizeNames(),
		flavorByImage: make(map[string]string, len(cfg.Flavors)),
		sizes:         make(map[string]corev1.ResourceRequirements, len(cfg.Sizes)),
		ceiling:       corev1.ResourceList{},
	}
	for _, flavorName := range c.flavorNames {
		c.flavorByImage[cfg.ImageRef(cfg.Flavors[flavorName].Image)] = flavorName
	}
	for _, sizeName := range c.sizeNames {
		size := cfg.Sizes[sizeName]
		parsed, err := containerspec.ParseResourceQuantities(size.Requests, size.Limits)
		if err != nil {
			return nil, fmt.Errorf("cfunc size %q: %w", sizeName, err)
		}
		c.sizes[sizeName] = parsed
		for resourceName, limit := range parsed.Limits {
			if current, ok := c.ceiling[resourceName]; !ok || limit.Cmp(current) > 0 {
				c.ceiling[resourceName] = limit
			}
		}
	}
	if !cfg.Enabled { // validated only when enabled
		return c, nil
	}
	var err error
	if c.fetcherResources, err = containerspec.ParseResourceQuantities(cfg.FetcherResources.Requests, cfg.FetcherResources.Limits); err != nil {
		return nil, fmt.Errorf("cfunc fetcherResources: %w", err)
	}
	if c.builderResources, err = containerspec.ParseResourceQuantities(cfg.BuilderResources.Requests, cfg.BuilderResources.Limits); err != nil {
		return nil, fmt.Errorf("cfunc builderResources: %w", err)
	}
	return c, nil
}

// trustedCA returns the volume and mount of the trusted CA ConfigMap, or two
// nils when none is configured. The ConfigMap is mounted whole, with no items
// and no subPath, so the kubelet updates the files of running pods when the
// bundle rotates, and a key is a file without being named here.
func (c *Catalog) trustedCA() (*corev1.Volume, *corev1.VolumeMount) {
	name := c.cfg.TrustedCA.ConfigMap
	if name == "" {
		return nil, nil
	}
	volume := &corev1.Volume{
		Name: trustedCAVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
		}},
	}
	mount := &corev1.VolumeMount{Name: trustedCAVolumeName, MountPath: TrustedCAMountPath, ReadOnly: true}
	return volume, mount
}
