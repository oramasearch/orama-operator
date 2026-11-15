/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import "testing"

// The mysql_native_password hash of "password", as MySQL and TiDB compute it.
func TestTiDBNativePasswordHashMatchesMySQL(t *testing.T) {
	if got := tidbNativePasswordHash("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Fatalf("hash = %s", got)
	}
}
