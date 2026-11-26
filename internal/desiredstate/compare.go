package desiredstate

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/pkg/api/drift"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Compare lists the fields in which live differs from desired, ordered by
// path. Labels and annotations are compared only for the keys desired names:
// controllers add their own, and those are not drift. Values are left out for
// Secrets.
func Compare(kind Kind, desired, live State) []drift.Field {
	c := &comparison{}
	for _, key := range unionKeys(desired.tree, live.tree) {
		path := "/" + escape(key)
		if key == "metadata" {
			c.metadata(path, desired.tree[key], live.tree[key])
			continue
		}
		c.values(path, desired.tree[key], live.tree[key])
	}
	for _, key := range unionKeys(desired.secret, live.secret) {
		want, wanted := desired.secret[key]
		got, exists := live.secret[key]
		if wanted != exists || !bytes.Equal(want, got) {
			c.add("/data/"+escape(key), nil, nil)
		}
	}
	if kind == KindSecret {
		for i := range c.fields {
			c.fields[i].Desired, c.fields[i].Live = nil, nil
		}
	}
	slices.SortStableFunc(c.fields, func(a, b drift.Field) int { return strings.Compare(a.Path, b.Path) })
	return c.fields
}

type comparison struct {
	fields []drift.Field
}

func (c *comparison) add(path string, desired, live any) {
	c.fields = append(c.fields, drift.Field{Path: path, Desired: desired, Live: live})
}

func (c *comparison) metadata(path string, desired, live any) {
	wantMeta, _ := desired.(map[string]any)
	gotMeta, _ := live.(map[string]any)
	for _, group := range unionKeys(wantMeta, gotMeta) {
		want, _ := wantMeta[group].(map[string]any)
		got, _ := gotMeta[group].(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(want)) {
			c.values(path+"/"+escape(group)+"/"+escape(key), want[key], got[key])
		}
	}
}

func (c *comparison) values(path string, desired, live any) {
	if isEmpty(desired) && isEmpty(live) {
		return
	}
	switch want := desired.(type) {
	case map[string]any:
		if got, ok := live.(map[string]any); ok {
			for _, key := range unionKeys(want, got) {
				c.values(path+"/"+escape(key), want[key], got[key])
			}
			return
		}
	case []any:
		if got, ok := live.([]any); ok && len(got) == len(want) {
			for i := range want {
				c.values(fmt.Sprintf("%s/%d", path, i), want[i], got[i])
			}
			return
		}
	}
	if !equalLeaf(path, desired, live) {
		c.add(path, desired, live)
	}
}

// isEmpty treats an absent value like an empty one: the API server and the Go
// types do not agree on which of the two they use.
func isEmpty(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	}
	return false
}

func equalLeaf(path string, desired, live any) bool {
	if reflect.DeepEqual(desired, live) {
		return true
	}
	want, wantOK := desired.(string)
	got, gotOK := live.(string)
	if !wantOK || !gotOK || !isQuantity(path) {
		return false
	}
	wantQuantity, err := resource.ParseQuantity(want)
	if err != nil {
		return false
	}
	gotQuantity, err := resource.ParseQuantity(got)
	return err == nil && wantQuantity.Cmp(gotQuantity) == 0
}

// isQuantity reports whether path holds a Kubernetes quantity, for which
// "1000m" and "1" are the same.
func isQuantity(path string) bool {
	return strings.Contains(path, "/resources/requests/") ||
		strings.Contains(path, "/resources/limits/") ||
		strings.Contains(path, "/capacity/") ||
		path == "/quota/cpu" || path == "/quota/memory"
}

func unionKeys[V any](a, b map[string]V) []string {
	keys := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		keys[key] = struct{}{}
	}
	for key := range b {
		keys[key] = struct{}{}
	}
	return slices.Sorted(maps.Keys(keys))
}

// escape encodes a key as a JSON pointer segment.
func escape(key string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
}
