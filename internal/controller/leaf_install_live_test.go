// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"os"
	"strings"
	"testing"
)

// These reach the network and, when LEAF_KUBECONFIG is set, read a real cluster. They change
// nothing. They are skipped unless asked for:
//
//	LEAF_LIVE=1 go test ./internal/controller/ -run Live -v
//	LEAF_LIVE=1 LEAF_OCI_CHART=registry-1.docker.io/oramasearch/orama-operator-chart:0.0.0-dev-abc1234 \
//	  LEAF_OCI_USER=... LEAF_OCI_PASS=... LEAF_KUBECONFIG=/path/to/kubeconfig go test ./internal/controller/ -run Live -v
func TestLiveChartDownloadAndPrerequisiteCheck(t *testing.T) {
	if os.Getenv("LEAF_LIVE") == "" {
		t.Skip("set LEAF_LIVE=1 to download real charts")
	}
	h := newHelmLeafInstaller()
	cnpg, err := h.fetchChart(chartSource{Repository: "https://cloudnative-pg.github.io/charts", Name: "cloudnative-pg", Version: "0.29.0"})
	if err != nil {
		t.Fatalf("a chart repository download: %v", err)
	}
	t.Logf("chart repository: %s %s (app %s), %d CRD files", cnpg.Metadata.Name, cnpg.Metadata.Version, cnpg.Metadata.AppVersion, len(cnpg.CRDObjects()))

	if _, err := h.fetchChart(chartSource{Repository: "https://cloudnative-pg.github.io/charts", Name: "cloudnative-pg", Version: "0.0.1-does-not-exist"}); err == nil {
		t.Fatal("a version that does not exist must be an error")
	} else {
		t.Logf("a missing version says: %v", err)
	}

	var required []string
	if ref := os.Getenv("LEAF_OCI_CHART"); ref != "" {
		repository, version, _ := strings.Cut(ref, ":")
		operator, err := h.fetchChart(chartSource{Repository: repository, Version: version, User: os.Getenv("LEAF_OCI_USER"), Pass: os.Getenv("LEAF_OCI_PASS")})
		if err != nil {
			t.Fatalf("an OCI chart pull: %v", err)
		}
		required = chartRequiredCRDs(operator)
		t.Logf("OCI: %s %s (app %s), %d CRD files, requires %v", operator.Metadata.Name, operator.Metadata.Version, operator.Metadata.AppVersion, len(operator.CRDObjects()), required)
	}
	if path := os.Getenv("LEAF_KUBECONFIG"); path != "" {
		kubeconfig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, c, err := leafClients(kubeconfig, "orama-system")
		if err != nil {
			t.Fatal(err)
		}
		missing, err := missingCRDs(context.Background(), c, append(required, "ciliumnetworkpolicies.cilium.io"))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("missing on the cluster: %v", missing)
	}
}
