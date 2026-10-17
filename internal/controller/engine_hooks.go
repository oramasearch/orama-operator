/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"strconv"
	"strings"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// prod1HookDeniedDomains is Prod 1's hooks deny list, entry for entry, as set on both sides
// (orama-deploy 655f1a2, oramacore/production/config/{writer,reader}.config.yaml): loopback,
// RFC 1918, link-local and cloud metadata, carrier-grade NAT, multicast and reserved, unspecified and
// broadcast, TEST-NETs, benchmarking and the listed private IPv6 patterns.
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
// by, plus IPv4-mapped and unspecified IPv6. The URL parser leaves an IPv4-mapped address in hex
// form ("[::ffff:10.0.0.1]" becomes "::ffff:a00:1"), so no IPv4 pattern sees it.
var clusterHookDeniedDomains = []string{
	"localhost", "*.svc", "*.svc.cluster.local", "*.cluster.local", "kubernetes*", "*.internal",
	"**.svc", "**.cluster.local", "kubernetes*.**", "**.internal",
	"::ffff:*", "::",
}

// Prod 1's hooks runtime timeouts, rendered as is on both sides.
const (
	hookEvaluationTimeout = "200ms"
	hookExecutionTimeout  = "1s"
)

// hookDeniedDomains is the deny list both engine sides render, as a fresh slice in insertion order
// without duplicates: Prod 1's entries, a "**" form of each of its "<prefix>.*" entries, the cluster
// names, then the CR's own Service names, short and qualified.
//
// orama-js-pool 0.4.3 (src/permission.rs, check_net_url and glob_match) matches the URL's host
// string, never the resolved address, and treats "." as a separator, so "*" stays within one label:
// Prod 1's "10.*" matches "10.0" but not "10.0.0.1", which "10.**" does. Prod 1's entries stay as
// they are so the list contains its own.
func hookDeniedDomains(oc *oramav1alpha1.OramaCloudV1) []string {
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range prod1HookDeniedDomains {
		add(d)
	}
	for _, d := range prod1HookDeniedDomains {
		if strings.HasSuffix(d, ".*") {
			add(d + "*")
		}
	}
	for _, d := range clusterHookDeniedDomains {
		add(d)
	}
	add(oc.Name + "-*")
	add(oc.Name + "-*.**")
	return out
}

// hooksYAML is the hooks mapping (OramaCore's HooksConfig, src/lib.rs) that writerSideYAML and
// readerSideYAML both embed, indented for a side section, so every config renders the same block and
// its config hash covers it. It sets denied_domains only: OramaCore refuses allowed_domains and
// denied_domains together, and with neither it lets hooks fetch any host.
func hooksYAML(oc *oramav1alpha1.OramaCloudV1) string {
	var b strings.Builder
	b.WriteString("  hooks:\n    denied_domains:\n")
	for _, d := range hookDeniedDomains(oc) {
		// Quoted: "*.svc" or "172.1[6-9].*" would not stay a plain string as a bare YAML scalar.
		fmt.Fprintf(&b, "      - %s\n", strconv.Quote(d))
	}
	fmt.Fprintf(&b, "    evaluation_timeout: %s\n    execution_timeout: %s\n", hookEvaluationTimeout, hookExecutionTimeout)
	return b.String()
}
