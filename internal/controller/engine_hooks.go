/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// prod1HookDeniedDomains is Prod 1's hooks deny list, entry for entry, as set on both sides
// (orama-deploy oramacore/production/config/{writer,reader}.config.yaml): loopback, RFC 1918,
// link-local and cloud metadata, carrier-grade NAT, multicast and reserved, TEST-NETs,
// benchmarking and private IPv6.
var prod1HookDeniedDomains = []string{
	"127.*", "localhost", "::1",
	"10.*", "172.1[6-9].*", "172.2[0-9].*", "172.3[0-1].*", "192.168.*",
	"169.254.169.254", "169.254.169.253", "169.254.169.123", "169.254.*", "fd00:ec2::*",
	"100.6[4-9].*", "100.[7-9][0-9].*", "100.1[0-1][0-9].*", "100.12[0-7].*",
	"22[4-9].*", "23[0-9].*", "24[0-9].*", "25[0-5].*", "255.255.255.255", "0.0.0.0",
	"192.0.0.*", "192.0.2.*", "198.51.100.*", "203.0.113.*", "198.1[8-9].*",
	"fc00:*", "fd00:*", "fe80:*", "ff00:*",
}

// clusterHookDeniedDomains are the in-cluster names a hook could reach a Service or the API server
// by, plus IPv4-mapped and unspecified IPv6, which the URL parser leaves in hex form
// ("[::ffff:10.0.0.1]" becomes "::ffff:a00:1") so no IPv4 pattern sees them.
var clusterHookDeniedDomains = []string{
	"localhost", "*.svc", "*.svc.cluster.local", "*.cluster.local", "kubernetes*", "*.internal",
	"**.svc", "**.cluster.local", "kubernetes*.**", "**.internal",
	"::ffff:*", "::",
}

// Hooks runtime timeouts when spec.components.engine.hooks leaves them unset: Prod 1's values,
// which are also OramaCore's own defaults.
const (
	defaultHookEvaluationTimeout = 200 * time.Millisecond
	defaultHookExecutionTimeout  = time.Second
)

// hookDeniedDomains is the deny list both engine sides render, in order and without duplicates:
// Prod 1's entries, then a "**" form of each of its "<prefix>.*" IPv4 entries, then the cluster
// names, the CR's own Service names, and spec.components.engine.hooks.extraDeniedDomains.
//
// orama-js-pool (0.4.3, src/permission.rs check_net_url and glob_match) matches the URL's host
// string, never the resolved address, and turns "." into a glob separator, so "*" stays within one
// label: Prod 1's "10.*" matches "10.0" but not "10.0.0.1". "10.**" spans the remaining labels,
// which is why the "**" forms are added; Prod 1's entries stay so the list is a superset of its own.
func hookDeniedDomains(oc *oramav1alpha1.OramaCloudV1) []string {
	var out []string
	seen := map[string]bool{}
	add := func(domains ...string) {
		for _, d := range domains {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	add(prod1HookDeniedDomains...)
	for _, d := range prod1HookDeniedDomains {
		if strings.HasSuffix(d, ".*") {
			add(d + "*")
		}
	}
	add(clusterHookDeniedDomains...)
	add(oc.Name+"-*", oc.Name+"-*.**")
	if h := oc.Spec.Components.Engine.Hooks; h != nil {
		add(h.ExtraDeniedDomains...)
	}
	return out
}

// hooksYAML is the hooks block (OramaCore's HooksConfig, oramacore src/lib.rs) that writerSideYAML
// and readerSideYAML both embed, so the two sides always render the same block. It sets
// denied_domains only: OramaCore refuses allowed_domains and denied_domains together, and with
// neither it lets hooks fetch any address.
func hooksYAML(oc *oramav1alpha1.OramaCloudV1) string {
	evaluation, execution := defaultHookEvaluationTimeout, defaultHookExecutionTimeout
	if h := oc.Spec.Components.Engine.Hooks; h != nil {
		evaluation = durationOr(h.EvaluationTimeout, evaluation)
		execution = durationOr(h.ExecutionTimeout, execution)
	}
	var b strings.Builder
	b.WriteString("  hooks:\n    denied_domains:\n")
	for _, d := range hookDeniedDomains(oc) {
		// Quoted: an entry such as "*.svc" or "[...]" means something else as a plain YAML scalar.
		fmt.Fprintf(&b, "      - %s\n", strconv.Quote(d))
	}
	fmt.Fprintf(&b, "    evaluation_timeout: %s\n    execution_timeout: %s\n", hookDuration(evaluation), hookDuration(execution))
	return b.String()
}

// hookDuration writes d as Prod 1 does ("200ms", "1s"), in whole milliseconds rounded up so a
// positive timeout never renders as zero.
func hookDuration(d time.Duration) string {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms%1000 == 0 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	return fmt.Sprintf("%dms", ms)
}
