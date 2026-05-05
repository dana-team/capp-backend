package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// Column describes one column in a table output.
// Wide=true columns are only shown when --output wide is used.
type Column[T any] struct {
	Header string
	Wide   bool
	Value  func(T) string
}

// PrintTable writes items as a tab-aligned table to w.
// Wide columns are included only when wide=true.
func PrintTable[T any](w io.Writer, cols []Column[T], items []T, wide bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	headers := make([]string, 0, len(cols))
	for _, c := range cols {
		if !c.Wide || wide {
			headers = append(headers, c.Header)
		}
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t")) //nolint:errcheck

	for _, item := range items {
		values := make([]string, 0, len(cols))
		for _, c := range cols {
			if !c.Wide || wide {
				values = append(values, c.Value(item))
			}
		}
		fmt.Fprintln(tw, strings.Join(values, "\t")) //nolint:errcheck
	}
	tw.Flush() //nolint:errcheck
}

// PrintJSON writes v as indented JSON to w.
func PrintJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// PrintYAML writes v as YAML to w.
func PrintYAML(w io.Writer, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling yaml: %w", err)
	}
	_, err = w.Write(data)
	return err
}
