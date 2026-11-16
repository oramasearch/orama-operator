// SPDX-License-Identifier: MIT

// crdunion writes the CRDs to apply before a new operator rolls onto a cluster: each chart CRD
// widened by every field the cluster's copy still declares (internal/crdrollout.Union). Applying
// them only ever adds, so the running operator loses nothing and the new one finds every field it
// reads. Apply chart/crds exactly once the new operator has replaced the old one.
//
//	kubectl --context <ctx> get crd -o json > cluster-crds.json
//	go run ./hack/crdunion -cluster cluster-crds.json -chart chart/crds -out <dir>
//	kubectl --context <ctx> apply --server-side --force-conflicts -f <dir>
//
// It reads nothing from a cluster itself and writes only to -out. docs/operations/crd-rollout.md
// has the whole order.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/oramasearch/orama-operator/internal/crdrollout"
)

func main() {
	clusterFile := flag.String("cluster", "", "the cluster's CRDs: the output of `kubectl get crd -o json`")
	chartDir := flag.String("chart", "chart/crds", "the chart's CRD directory")
	outDir := flag.String("out", "", "the directory to write the widened CRDs to (created; must not exist)")
	flag.Parse()
	if *clusterFile == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: crdunion -cluster <kubectl get crd -o json> [-chart chart/crds] -out <dir>")
		os.Exit(2)
	}
	widened, err := run(*clusterFile, *chartDir, *outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crdunion:", err)
		os.Exit(1)
	}
	if len(widened) == 0 {
		fmt.Println("crdunion: no chart CRD drops anything the cluster has; the widened CRDs equal the chart's")
		return
	}
	fmt.Printf("crdunion: %d CRD(s) keep fields the cluster has until the new operator runs: %s\n",
		len(widened), strings.Join(widened, ", "))
}

// run writes one file per chart CRD and returns the names of those widened beyond the chart.
func run(clusterFile, chartDir, outDir string) ([]string, error) {
	data, err := os.ReadFile(clusterFile)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s is not the JSON of a CRD list: %w", clusterFile, err)
	}
	onCluster := map[string]map[string]any{}
	for _, crd := range list.Items {
		onCluster[crdrollout.Name(crd)] = crd
	}
	files, err := filepath.Glob(filepath.Join(chartDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no CRDs in %s", chartDir)
	}
	if err := os.Mkdir(outDir, 0o755); err != nil {
		return nil, err
	}
	var widened []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		crds, err := crdrollout.Parse(file, raw)
		if err != nil {
			return nil, err
		}
		for _, crd := range crds {
			name := crdrollout.Name(crd)
			cluster := onCluster[name]
			if crdrollout.Wider(crd, cluster) {
				widened = append(widened, name)
			}
			out, err := yaml.Marshal(crdrollout.Union(crd, cluster))
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(outDir, name+".yaml"), out, 0o644); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(widened)
	return widened, nil
}
