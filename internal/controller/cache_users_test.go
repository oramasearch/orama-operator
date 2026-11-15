/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strings"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The ACL file turns the default user off and gives no client @all, a wildcard key pattern, the
// admin password or another client's password (#825).
func TestCacheACLTemplateIsLeastPrivilege(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(cacheACLTemplate()), "\n")
	if len(lines) != 2+len(cacheClients) || lines[0] != "user default off resetkeys resetchannels -@all" {
		t.Fatalf("ACL file = %q", lines)
	}
	if !strings.HasPrefix(lines[1], "user admin on >"+keyPlaceholder(redisPasswordEnv)+" ") {
		t.Fatalf("admin line = %q", lines[1])
	}
	for i, c := range cacheClients {
		line := lines[2+i]
		if !strings.HasPrefix(line, "user "+c.user+" on >"+keyPlaceholder(cacheClientPasswordEnv(c.user))+" resetchannels ") {
			t.Errorf("%s line = %q", c.user, line)
		}
		for _, never := range []string{"+@all", "~* ", "allkeys", "allcommands", keyPlaceholder(redisPasswordEnv)} {
			if strings.Contains(line+" ", never) {
				t.Errorf("%s line holds %q: %s", c.user, never, line)
			}
		}
		if !strings.Contains(line, " -@all +@connection -@admin -@dangerous ") {
			t.Errorf("%s line does not start from no commands: %s", c.user, line)
		}
	}
}

// An external or central cache is not the operator's Redis: its clients keep the supplied
// <cr>-cache-auth and a URL without a user; an in-cluster cache gives each client its own.
func TestCacheClientsUseTheirOwnUserOnlyWhereRedisRunsHere(t *testing.T) {
	local := stackOC(nil)
	if got := redisClientURL(local, cacheClientIndexBuilder); got != "redis://index-builder:$(REDIS_PASSWORD)@v1-cache:6379" {
		t.Errorf("in-cluster URL = %q", got)
	}
	if got := redisClientSecret(local, cacheClientHarvestMaster); got != "v1-cache-harvest-master" {
		t.Errorf("in-cluster Secret = %q", got)
	}
	external := stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Cache = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external"}
	})
	if got := redisClientURL(external, cacheClientIndexBuilder); got != redisURL(external) {
		t.Errorf("external URL = %q", got)
	}
	if got := redisClientSecret(external, cacheClientRedisHTTP); got != "v1-cache-auth" {
		t.Errorf("external Secret = %q", got)
	}
}
