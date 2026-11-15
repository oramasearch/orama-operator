// SPDX-License-Identifier: MIT

package controller

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestLakeEnvironmentRBACAndChartCRDArePackaged(t *testing.T) {
	generatedRBAC, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chartRBAC, err := os.ReadFile("../../chart/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, resourceName := range []string{
		"lakeenvironments",
		"lakeenvironments/status",
		"lakeenvironments/finalizers",
		"networkpolicies",
		"resourcequotas",
		"limitranges",
	} {
		if !strings.Contains(string(generatedRBAC), resourceName) {
			t.Errorf("generated RBAC is missing %s", resourceName)
		}
		if !strings.Contains(string(chartRBAC), resourceName) {
			t.Errorf("chart RBAC is missing %s", resourceName)
		}
	}

	generatedCRD, err := os.ReadFile("../../config/crd/bases/orama.cloud_lakeenvironments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chartCRD, err := os.ReadFile("../../chart/crds/orama.cloud_lakeenvironments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generatedCRD, chartCRD) {
		t.Fatal("chart LakeEnvironment CRD differs from generated CRD")
	}
}

func TestLakeAppRBACAndChartCRDArePackaged(t *testing.T) {
	generatedRBAC, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chartRBAC, err := os.ReadFile("../../chart/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, resourceName := range []string{
		"lakeapps",
		"lakeapps/status",
		"lakeapps/finalizers",
		"pods",
		"networkpolicies",
		"resourcequotas",
		"limitranges",
	} {
		if !strings.Contains(string(generatedRBAC), resourceName) {
			t.Errorf("generated RBAC is missing %s", resourceName)
		}
		if !strings.Contains(string(chartRBAC), resourceName) {
			t.Errorf("chart RBAC is missing %s", resourceName)
		}
	}

	generatedCRD, err := os.ReadFile("../../config/crd/bases/orama.cloud_lakeapps.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chartCRD, err := os.ReadFile("../../chart/crds/orama.cloud_lakeapps.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generatedCRD, chartCRD) {
		t.Fatal("chart LakeApp CRD differs from generated CRD")
	}
}
