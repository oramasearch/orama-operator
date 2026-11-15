/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Rows are admitted in lake's order until a key has its limit, per key.
func TestRowLimitAdmitsPerKeyInOrder(t *testing.T) {
	limit := newRowLimit(2, "things per workspace")
	for i, want := range []bool{true, true, false, false} {
		if got := limit.admit("ws-a"); got != want {
			t.Fatalf("row %d of ws-a admitted %t, want %t", i, got, want)
		}
	}
	if !limit.admit("ws-b") {
		t.Fatal("another workspace was refused for the first one's rows")
	}
}

// A customer's services past the limit are refused, workspace Zettas counted, others' unaffected,
// and a service already Ready is never refused.
func TestServicesOverCustomerLimit(t *testing.T) {
	var services []LakeService
	for i := 0; i < lakeServicesPerCustomer+2; i++ {
		services = append(services, LakeService{ID: "a" + strconv.Itoa(i), CustomerID: "cust-a", Desired: serviceDesiredPresent})
	}
	services[0].Kind, services[0].WorkspaceZetta = serviceKindZetta, true
	services[len(services)-1].Status = &LakeServiceStatus{Phase: ServicePhaseReady}
	services = append(services, LakeService{ID: "b0", CustomerID: "cust-b", Desired: serviceDesiredPresent})
	over := servicesOverCustomerLimit(services)
	if len(over) != 1 || !over["a"+strconv.Itoa(lakeServicesPerCustomer)] {
		t.Fatalf("over the limit: %v, want only the first unready service past it", over)
	}
}

// lake gets the same limits, which spec.env cannot change.
func TestLakeGetsTheLimitsAndSpecEnvCannotChangeThem(t *testing.T) {
	lake := bareLake()
	lake.Spec.Env = []corev1.EnvVar{{Name: "LAKE_WORKSPACES_PER_CUSTOMER", Value: "0"}}
	values := map[string][]string{}
	for _, variable := range lakeEnv(lake, lakeSealingMaterial{}) {
		values[variable.Name] = append(values[variable.Name], variable.Value)
	}
	for _, want := range lakeLimitEnv() {
		if got := values[want.Name]; len(got) != 1 || got[0] != want.Value {
			t.Fatalf("%s = %v, want only %q", want.Name, got, want.Value)
		}
	}
}
