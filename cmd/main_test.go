/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import "testing"

func TestLeaderElectionIDPreservesProductionLease(t *testing.T) {
	const productionLease = "orama-cloud-v1-operator.orama.cloud"
	if leaderElectionID != productionLease {
		t.Fatalf("leader election ID = %q, want preserved production Lease %q", leaderElectionID, productionLease)
	}
}
