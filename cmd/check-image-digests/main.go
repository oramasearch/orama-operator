// check-image-digests rejects mutable executable image references in rendered release manifests.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/oramasearch/orama-operator/internal/imagepolicy"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func main() {
	if err := check(os.Stdin, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(input io.Reader, output io.Writer) error {
	decoder := yaml.NewYAMLOrJSONDecoder(input, 4096)
	failures, workloads := 0, 0
	for document := 1; ; document++ {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("document %d: %w", document, err)
		}
		workloads += imagepolicy.WorkloadCount(object)
		for _, path := range imagepolicy.MutableImages(object) {
			fmt.Fprintf(output, "document %d (%v): %s requires a repository@sha256 digest\n", document, object["kind"], path)
			failures++
		}
	}
	if workloads == 0 {
		return fmt.Errorf("no rendered executable workloads found")
	}
	if failures > 0 {
		return fmt.Errorf("%d mutable image selections", failures)
	}
	return nil
}
