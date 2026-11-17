// Package boundcapps holds the bound-Capps response DTO of the REST API.
//
// It imports nothing, so clients (cappctl, capp-mcp) can share it without
// pulling in the server package internal/resources/namespaced/boundcapps and
// its Kubernetes dependencies. That package aliases it.
package boundcapps

// Response is returned by GET .../secrets/:name/bound and .../configmaps/:name/bound.
type Response struct {
	// BoundCapps lists every Capp in the namespace that references the resource,
	// from its pod template or (Secrets only) its log settings.
	BoundCapps []string `json:"boundCapps"`
	// ToReload lists the subset of BoundCapps that restart when the resource
	// changes: opted-in Capps whose pod template references it.
	ToReload []string `json:"toReload"`
	// BoundFunctions lists every cfunc function in the namespace that reads
	// the resource as files. Deleting the resource is refused while it is
	// non-empty. It is always empty on a backend that does not serve cfunc
	// functions.
	BoundFunctions []string `json:"boundFunctions"`
}
