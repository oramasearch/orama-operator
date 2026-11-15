/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Redis ACL users, one per cache client (#825, principle 4). Where the cache runs in this cluster,
// Redis loads an ACL file the operator renders: the default user is off, an operator-only admin user
// holds <cr>-cache-auth's password (for recovery, never given to a client), and each client signs in
// as its own user with only its key patterns and commands, its password generated into
// <cr>-cache-<client>. The patterns and commands are read from each client's source
// (orama-cloud-v1-bridge):
//
//   - index-builder (index-manager/apps/index-builder/src/embeddings/cache.ts): MGET and MSET on its
//     embedding cache, keys v1:*.
//   - harvest-master (HARVEST/master/app.js): GET, SET (with EX), DEL and KEYS on job:* and
//     heartbeat:*. KEYS is in @dangerous and is named for it alone; Redis applies no key pattern to
//     it, so it lists every key name (never a value).
//   - redis-http, the serverless-redis-http shim in front of the dashboard's @upstash/redis client
//     (dashboard src/app/api/projects/[id]/analytics/*-get-body/route.ts): GET and SET (with EX) on
//     project:*. The shim forwards whatever the dashboard sends, so its user, not the shim, bounds it.
//
// Every user also gets @connection minus @admin and @dangerous (AUTH, HELLO, PING, SELECT, CLIENT
// SETNAME/SETINFO), which the clients send on connect. Elsewhere (an external or central cache) the
// operator runs no Redis, and the clients keep the supplied <cr>-cache-auth.

// cacheClient is one Redis client's ACL user.
type cacheClient struct {
	user     string
	keys     []string
	commands []string
}

// The cache clients, by Redis user name.
const (
	cacheClientIndexBuilder  = "index-builder"
	cacheClientHarvestMaster = "harvest-master"
	cacheClientRedisHTTP     = redisHTTPComponent
	// cacheAdminUser is the operator-only user that holds <cr>-cache-auth's password.
	cacheAdminUser = "admin"
	// cacheACLFile is the rendered ACL file's name, mounted at cacheACLMount.
	cacheACLFile  = "users.acl"
	cacheACLMount = "/etc/redis"
)

var cacheClients = []cacheClient{
	{user: cacheClientIndexBuilder, keys: []string{"v1:*"}, commands: []string{"mget", "mset"}},
	{user: cacheClientHarvestMaster, keys: []string{"job:*", "heartbeat:*"}, commands: []string{"get", "set", "del", "keys"}},
	{user: cacheClientRedisHTTP, keys: []string{"project:*"}, commands: []string{"get", "set"}},
}

// cacheUsersLocal reports whether the stack's Redis loads the ACL file: wherever the cache runs in
// this cluster.
func cacheUsersLocal(oc *oramav1alpha1.OramaCloudV1) bool {
	return storeLocal(oc, oc.Spec.Components.Cache)
}

// cacheClientSecretName is <cr>-cache-<user>, the client's generated password (key password).
func cacheClientSecretName(oc *oramav1alpha1.OramaCloudV1, user string) string {
	return oc.Name + "-cache-" + user
}

// cacheClientPasswordEnv is the template variable carrying a client's password into the ACL file.
func cacheClientPasswordEnv(user string) string {
	return "CACHE_" + strings.ToUpper(strings.ReplaceAll(user, "-", "_")) + "_PASSWORD"
}

// cacheACLTemplate is the ACL file, its passwords __VAR__ placeholders the render init container
// fills. The default user is defined, and off: an ACL file that omits it would leave it on with no
// password and every permission.
func cacheACLTemplate() string {
	var b strings.Builder
	b.WriteString("user default off resetkeys resetchannels -@all\n")
	fmt.Fprintf(&b, "user %s on >%s ~* &* +@all\n", cacheAdminUser, keyPlaceholder(redisPasswordEnv))
	for _, c := range cacheClients {
		fmt.Fprintf(&b, "user %s on >%s resetchannels", c.user, keyPlaceholder(cacheClientPasswordEnv(c.user)))
		for _, k := range c.keys {
			b.WriteString(" ~" + k)
		}
		b.WriteString(" -@all +@connection -@admin -@dangerous")
		for _, cmd := range c.commands {
			b.WriteString(" +" + cmd)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// cacheACLTemplateEnv is the ACL file's variables: the admin password from <cr>-cache-auth and each
// client's from its own Secret.
func cacheACLTemplateEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	env := []corev1.EnvVar{redisPasswordEnvVar(oc)}
	for _, c := range cacheClients {
		env = append(env, secretKeyEnv(cacheClientPasswordEnv(c.user), cacheClientSecretName(oc, c.user), "password"))
	}
	return env
}

// cacheServerSecrets is every Secret the ACL file is rendered from, for the cache's credentials hash.
func cacheServerSecrets(oc *oramav1alpha1.OramaCloudV1) []string {
	names := []string{cacheAuthSecretName(oc)}
	for _, c := range cacheClients {
		names = append(names, cacheClientSecretName(oc, c.user))
	}
	return names
}

// redisClientEnv is a client's REDIS_PASSWORD: its own user's where the ACL file is loaded,
// otherwise the supplied <cr>-cache-auth.
func redisClientEnv(oc *oramav1alpha1.OramaCloudV1, user string) corev1.EnvVar {
	if !cacheUsersLocal(oc) {
		return redisPasswordEnvVar(oc)
	}
	return secretKeyEnv(redisPasswordEnv, cacheClientSecretName(oc, user), "password")
}

// redisClientURL is a client's Redis URL, its password a $(REDIS_PASSWORD) reference to an earlier
// redisClientEnv entry, and its user name where the ACL file is loaded.
func redisClientURL(oc *oramav1alpha1.OramaCloudV1, user string) string {
	if !cacheUsersLocal(oc) {
		return redisURL(oc)
	}
	return "redis://" + user + ":$(" + redisPasswordEnv + ")@" + oc.Name + "-cache:6379"
}

// redisClientSecret is the Secret a client's Redis password comes from, for its credentials hash.
func redisClientSecret(oc *oramav1alpha1.OramaCloudV1, user string) string {
	if !cacheUsersLocal(oc) {
		return cacheAuthSecretName(oc)
	}
	return cacheClientSecretName(oc, user)
}
