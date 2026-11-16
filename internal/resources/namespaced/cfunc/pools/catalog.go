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

	flavorNames   []string
	sizeNames     []string
	flavorByImage map[string]string
	sizes         map[string]corev1.ResourceRequirements
	ceiling       corev1.ResourceList
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
	return c, nil
}
