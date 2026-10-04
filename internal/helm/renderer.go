// Package helm shells out to the helm binary to render charts into Kubernetes
// manifests without creating a Helm release (equivalent to helm template).
package helm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	helmAnnotationPrefix = "helm.sh/"

	// LabelBenchmark is the label applied to every rendered benchmark object
	// to enable efficient list/delete via K8s label selectors.
	LabelBenchmark = "capp.dana.io/benchmark"

	// AnnotationCappName is the annotation that records which capp the
	// benchmark targets.
	AnnotationCappName = "capp.dana.io/capp-name"
)

// RenderOpts holds the parameters for a single render call.
type RenderOpts struct {
	ReleaseName string
	Namespace   string
	Values      map[string]interface{}
}

// Renderer shells out to the helm binary to render a chart into Kubernetes
// manifests.
type Renderer struct {
	chartRef string
	logger   *zap.Logger
}

// NewRenderer returns a Renderer for the given chart reference.
func NewRenderer(chartRef string, logger *zap.Logger) *Renderer {
	return &Renderer{chartRef: chartRef, logger: logger}
}

// Render runs helm template, parses the output into unstructured objects,
// and strips Helm-specific annotations.
func (r *Renderer) Render(ctx context.Context, opts RenderOpts) ([]unstructured.Unstructured, error) {
	args := []string{"template", opts.ReleaseName, r.chartRef,
		"--namespace", opts.Namespace,
	}

	if len(opts.Values) > 0 {
		valuesFile, err := writeValuesFile(opts.Values)
		if err != nil {
			return nil, fmt.Errorf("write values file: %w", err)
		}
		defer os.Remove(valuesFile) //nolint:errcheck // best-effort cleanup
		args = append(args, "--values", valuesFile)
	}

	r.logger.Debug("running helm template",
		zap.String("chartRef", r.chartRef),
		zap.String("releaseName", opts.ReleaseName),
		zap.String("namespace", opts.Namespace),
	)

	cmd := exec.CommandContext(ctx, "helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm template: %w: %s", err, stderr.String())
	}

	objects, err := ParseManifests(stdout.String())
	if err != nil {
		return nil, fmt.Errorf("parse manifests: %w", err)
	}

	StripHelmAnnotations(objects)
	return objects, nil
}

// writeValuesFile writes values to a temporary JSON file and returns its path.
// The caller is responsible for removing the file.
func writeValuesFile(values map[string]interface{}) (string, error) {
	f, err := os.CreateTemp("", "helm-values-*.json")
	if err != nil {
		return "", err
	}

	if err := json.NewEncoder(f).Encode(values); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}

// ParseManifests splits a multi-document YAML string into individual
// unstructured Kubernetes objects. Empty and comment-only documents are skipped.
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

// StripHelmAnnotations removes all helm.sh/ annotations from the given
// objects. If no annotations remain, the map is removed entirely.
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

// FilterByGVK returns only objects matching the given GroupVersionKind.
func FilterByGVK(objects []unstructured.Unstructured, gvk schema.GroupVersionKind) []unstructured.Unstructured {
	var filtered []unstructured.Unstructured
	for _, obj := range objects {
		if obj.GroupVersionKind() == gvk {
			filtered = append(filtered, obj)
		}
	}
	return filtered
}

// SetBenchmarkMeta adds the benchmark label and capp-name annotation to
// every object for listing, monitoring, and cleanup.
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
