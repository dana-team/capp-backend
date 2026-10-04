// Package helm provides a Helm chart renderer that pulls charts from OCI
// registries and renders them into Kubernetes manifests without creating a
// Helm release (equivalent to `helm template`). This is the same approach
// ArgoCD uses: render locally, then apply the resulting objects via the K8s API.
package helm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// helmAnnotationPrefix is the prefix for Helm-managed annotations that are
	// stripped from rendered manifests before they are applied to the cluster.
	helmAnnotationPrefix = "helm.sh/"

	// LabelBenchmark is a label set on every rendered benchmark object.
	// It enables efficient K8s list/delete via label selectors.
	LabelBenchmark = "capp.dana.io/benchmark"

	// AnnotationCappName records which capp the benchmark targets.
	AnnotationCappName = "capp.dana.io/capp-name"
)

// Renderer renders a Helm chart into Kubernetes manifests without creating
// a release (equivalent to `helm template`).
type Renderer interface {
	// Render pulls the chart (caching it after the first pull), renders it
	// with the given options, parses the output into Kubernetes objects, and
	// strips Helm-specific annotations.
	Render(ctx context.Context, opts RenderOpts) ([]unstructured.Unstructured, error)
}

// RenderOpts controls how the chart is rendered.
type RenderOpts struct {
	// ReleaseName is the Helm release name used during rendering.
	// It affects template values like {{ .Release.Name }}.
	ReleaseName string

	// Namespace is the target namespace for the rendered manifests.
	Namespace string

	// Values is the set of values passed to the chart templates,
	// equivalent to `helm template --values`.
	Values map[string]interface{}
}

// NewRenderer creates a Renderer backed by an OCI chart reference.
// The chart is pulled lazily on the first Render call and cached in memory.
func NewRenderer(chartRef string, logger *zap.Logger) Renderer {
	return &ociRenderer{chartRef: chartRef, logger: logger}
}

// NewRendererFromChart creates a Renderer from a pre-loaded chart.
// This is intended for testing; no OCI pull is performed.
func NewRendererFromChart(ch *helmchart.Chart, logger *zap.Logger) Renderer {
	return &ociRenderer{chart: ch, logger: logger}
}

type ociRenderer struct {
	chartRef string
	logger   *zap.Logger

	mu    sync.Mutex
	chart *helmchart.Chart
}

func (r *ociRenderer) Render(ctx context.Context, opts RenderOpts) ([]unstructured.Unstructured, error) {
	ch, err := r.loadChart()
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}

	install := action.NewInstall(new(action.Configuration))
	install.DryRun = true
	install.ClientOnly = true
	install.ReleaseName = opts.ReleaseName
	install.Namespace = opts.Namespace
	install.Replace = true
	install.IncludeCRDs = false

	rel, err := install.RunWithContext(ctx, ch, opts.Values)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	// Helm puts hook templates in rel.Hooks rather than rel.Manifest.
	// Since we apply objects via K8s API (not Helm install), we need both.
	var allManifests strings.Builder
	allManifests.WriteString(rel.Manifest)
	for _, hook := range rel.Hooks {
		allManifests.WriteString("\n---\n")
		allManifests.WriteString(hook.Manifest)
	}

	objects, err := ParseManifests(allManifests.String())
	if err != nil {
		return nil, fmt.Errorf("parse manifests: %w", err)
	}

	StripHelmAnnotations(objects)
	return objects, nil
}

// loadChart returns the cached chart or pulls it from OCI on first call.
func (r *ociRenderer) loadChart() (*helmchart.Chart, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.chart != nil {
		return r.chart, nil
	}

	r.logger.Info("pulling chart from OCI registry", zap.String("ref", r.chartRef))

	regClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create registry client: %w", err)
	}

	result, err := regClient.Pull(r.chartRef)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", r.chartRef, err)
	}

	ch, err := loader.LoadArchive(bytes.NewReader(result.Chart.Data))
	if err != nil {
		return nil, fmt.Errorf("load chart archive: %w", err)
	}

	r.chart = ch
	r.logger.Info("chart cached",
		zap.String("name", ch.Metadata.Name),
		zap.String("version", ch.Metadata.Version),
	)

	return ch, nil
}

// ParseManifests splits a multi-document YAML string (as returned by Helm)
// into individual unstructured Kubernetes objects. Empty documents and
// documents that do not parse into objects (e.g. comment-only) are skipped.
func ParseManifests(manifest string) ([]unstructured.Unstructured, error) {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))

	var objects []unstructured.Unstructured

	for {
		data, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read YAML document: %w", err)
		}

		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}

		var obj unstructured.Unstructured
		if err := sigsyaml.Unmarshal(data, &obj.Object); err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		objects = append(objects, obj)
	}

	return objects, nil
}

// StripHelmAnnotations removes all annotations with the "helm.sh/" prefix
// from the given objects. If removing Helm annotations leaves the annotation
// map empty, it is deleted entirely.
func StripHelmAnnotations(objects []unstructured.Unstructured) {
	for i := range objects {
		annotations := objects[i].GetAnnotations()
		if len(annotations) == 0 {
			continue
		}

		changed := false
		for k := range annotations {
			if strings.HasPrefix(k, helmAnnotationPrefix) {
				delete(annotations, k)
				changed = true
			}
		}

		if !changed {
			continue
		}

		if len(annotations) == 0 {
			objects[i].SetAnnotations(nil)
		} else {
			objects[i].SetAnnotations(annotations)
		}
	}
}

// FilterByGVK returns only objects whose GroupVersionKind matches gvk.
func FilterByGVK(objects []unstructured.Unstructured, gvk schema.GroupVersionKind) []unstructured.Unstructured {
	var filtered []unstructured.Unstructured
	for _, obj := range objects {
		if obj.GroupVersionKind() == gvk {
			filtered = append(filtered, obj)
		}
	}
	return filtered
}

// SetBenchmarkMeta stamps every object with the benchmark label and
// annotation so they can be efficiently listed, monitored, and cleaned up.
//
// Label (queryable via selectors):
//
//	capp.dana.io/benchmark: "true"
//
// Annotation (machine metadata):
//
//	capp.dana.io/capp-name: <cappName>
func SetBenchmarkMeta(objects []unstructured.Unstructured, cappName string) {
	for i := range objects {
		labels := objects[i].GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[LabelBenchmark] = "true"
		objects[i].SetLabels(labels)

		annotations := objects[i].GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationCappName] = cappName
		objects[i].SetAnnotations(annotations)
	}
}
