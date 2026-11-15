/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestPgAdminEmail: pgAdmin's login is spec.components.pgAdmin.email, and a neutral address when it
// is empty, never a person's.
func TestPgAdminEmail(t *testing.T) {
	oc := &oramav1alpha1.OramaCloudV1{}
	if got := pgAdminEmail(oc); got != "admin@example.com" {
		t.Fatalf("default pgAdmin email = %q, want admin@example.com", got)
	}
	oc.Spec.Components.PgAdmin.Email = "dba@company.example"
	if got := pgAdminEmail(oc); got != "dba@company.example" {
		t.Fatalf("pgAdmin email = %q, want the spec's", got)
	}
}
