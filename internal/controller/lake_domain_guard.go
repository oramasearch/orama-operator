/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lakeDomainGuard is what a tenant's custom domain may never be (#776): a platform host, or a host
// something else on the cluster already serves. The operator refuses these on its own, whatever
// lake's create_domain accepted (lake#949).
type lakeDomainGuard struct {
	// platform names each name a domain may not equal or be beneath, with the Lake it belongs to.
	platform map[string]string
	// taken names each exact host a LakeApp, or an environment of another Lake, already serves.
	taken map[string]lakeDomainTaken
}

// lakeDomainTaken is who serves a host: why it is refused, and the environment of this Lake that
// may still claim it (its own named host), if any.
type lakeDomainTaken struct {
	by       string
	identity string
}

// lakePlatformNames are the names a Lake owns, every name beneath each included: its host, public
// host and apex host, its apps and preview wildcards, and, when it has a DNS token, the zone of
// each of its hosts. The token writes in that zone, and Cloudflare zones are registrable domains, so
// the zone is the host's registrable domain (lake.live for eu.lake.live, oramadev.net for
// lake-dev.oramadev.net).
func lakePlatformNames(lake *oramav1alpha1.Lake) []string {
	hosts := []string{lake.Spec.Host, lakePublicHost(lake), lake.Spec.ApexHost}
	names := append([]string(nil), hosts...)
	names = append(names, lakeAppsWildcard(lake), strings.TrimPrefix(lake.Spec.PreviewWildcard, "*."))
	if lakeHasDNSCredential(lake) {
		for _, host := range hosts {
			if zone, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
				names = append(names, zone)
			}
		}
	}
	var out []string
	for _, name := range names {
		if name = strings.TrimSuffix(strings.ToLower(name), "."); name != "" && strings.Contains(name, ".") {
			out = append(out, name)
		}
	}
	return out
}

// newLakeDomainGuard is the guard for lake's domains: the platform names of lake and of every Lake
// on the cluster, the host of every LakeApp, and every host and domain of the environments of the
// other Lakes. This Lake's own environments' domains are its claims, decided by the first-claim
// rule, so only their named hosts are taken.
func newLakeDomainGuard(lake *oramav1alpha1.Lake, lakes []oramav1alpha1.Lake, apps []oramav1alpha1.LakeApp, environments []oramav1alpha1.LakeEnvironment) lakeDomainGuard {
	guard := lakeDomainGuard{platform: map[string]string{}, taken: map[string]lakeDomainTaken{}}
	everyLake := append([]oramav1alpha1.Lake{*lake}, lakes...)
	for i := range everyLake {
		for _, name := range lakePlatformNames(&everyLake[i]) {
			if _, found := guard.platform[name]; !found {
				guard.platform[name] = everyLake[i].Name
			}
		}
	}
	for _, app := range apps {
		if app.Spec.Host != "" {
			guard.taken[strings.ToLower(app.Spec.Host)] = lakeDomainTaken{by: fmt.Sprintf("the host of LakeApp %s/%s", app.Namespace, app.Name)}
		}
	}
	for _, environment := range environments {
		owner := fmt.Sprintf("LakeEnvironment %s/%s", environment.Namespace, environment.Name)
		ours := environment.Spec.LakeRef == lake.Name
		if environment.Spec.Host != "" {
			taken := lakeDomainTaken{by: "the host of " + owner}
			if ours {
				taken.identity = platformEnvironmentKey(environment.Spec.Repository, environment.Spec.Environment)
			}
			guard.taken[strings.ToLower(environment.Spec.Host)] = taken
		}
		if ours {
			continue
		}
		for _, domain := range environment.Spec.Domains {
			guard.taken[strings.ToLower(domain.Host)] = lakeDomainTaken{by: "a domain of " + owner + " of another Lake"}
		}
	}
	return guard
}

// refuses is why domain may not be a custom domain of identity (an environment of lake), or ""
// when it may. lake's own names are always checked, even when the guard was built without it.
func (g lakeDomainGuard) refuses(lake *oramav1alpha1.Lake, identity, domain string) string {
	platform := make(map[string]string, len(g.platform))
	for name, owner := range g.platform {
		platform[name] = owner
	}
	for _, name := range lakePlatformNames(lake) {
		if _, found := platform[name]; !found {
			platform[name] = lake.Name
		}
	}
	names := make([]string, 0, len(platform))
	for name := range platform {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if domain == name || strings.HasSuffix(domain, "."+name) {
			return fmt.Sprintf("%q is a platform host of Lake %q (%s or a name beneath it)", domain, platform[name], name)
		}
	}
	if taken, found := g.taken[domain]; found && taken.identity != identity {
		return fmt.Sprintf("%q is already %s", domain, taken.by)
	}
	return ""
}

// lakeDomainGuardFor reads the cluster's Lakes, LakeApps and LakeEnvironments for lake's guard.
func lakeDomainGuardFor(ctx context.Context, c client.Reader, lake *oramav1alpha1.Lake) (lakeDomainGuard, error) {
	var lakes oramav1alpha1.LakeList
	if err := c.List(ctx, &lakes); err != nil {
		return lakeDomainGuard{}, fmt.Errorf("list Lakes: %w", err)
	}
	var apps oramav1alpha1.LakeAppList
	if err := c.List(ctx, &apps); err != nil {
		return lakeDomainGuard{}, fmt.Errorf("list LakeApps: %w", err)
	}
	var environments oramav1alpha1.LakeEnvironmentList
	if err := c.List(ctx, &environments); err != nil {
		return lakeDomainGuard{}, fmt.Errorf("list LakeEnvironments: %w", err)
	}
	return newLakeDomainGuard(lake, lakes.Items, apps.Items, environments.Items), nil
}
