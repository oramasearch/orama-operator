/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// THE RULE (CLAUDE.md, "Two installation paths, one result"): the management cluster is installed
// from helmfile.yaml, and every leaf by the management cluster's operator from the
// ClusterHelmRelease resources in config/clusters/<cluster>/platform.yaml. The two paths must
// install the same charts at the same versions, in the same namespaces, with the same values, so
// that a management cluster and a leaf are effectively the same cluster. Steve, 2026-10-13: "both
// the helmfile and the operator need to be kept in sync".
//
// This test is what keeps them in step. It fails when a chart, a version, a namespace, a value, a
// dependency or a separately-shipped CRD manifest is in one and not the other.

// platformRelease is one chart release, described the same way whichever path it came from.
type platformRelease struct {
	// Release is the Helm release name. Two paths that install the same chart under different
	// release names would leave a cluster with two copies of it (operator#753).
	Release    string
	Repository string
	Chart      string
	Version    string
	Namespace  string
	Values     map[string]any
	DependsOn  []string // chart names
	CRDURLs    []string
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// setValue puts a helmfile `set` entry (a dotted path and a value) into nested values, with the
// types Helm's --set gives: true and false are booleans.
func setValue(values map[string]any, path string, value any) {
	if text, ok := value.(string); ok {
		switch text {
		case "true":
			value = true
		case "false":
			value = false
		}
	}
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		next, _ := values[part].(map[string]any)
		if next == nil {
			next = map[string]any{}
			values[part] = next
		}
		values = next
	}
	values[parts[len(parts)-1]] = value
}

func mergeValues(into, from map[string]any) {
	for key, value := range from {
		base, baseIsMap := into[key].(map[string]any)
		over, overIsMap := value.(map[string]any)
		if baseIsMap && overIsMap {
			mergeValues(base, over)
			continue
		}
		into[key] = value
	}
}

// helmfilePlatform reads the third-party releases of helmfile.yaml, by chart name.
func helmfilePlatform(t *testing.T) map[string]platformRelease {
	t.Helper()
	var helmfile struct {
		Repositories []struct{ Name, URL string } `json:"repositories"`
		Releases     []struct {
			Name      string           `json:"name"`
			Namespace string           `json:"namespace"`
			Chart     string           `json:"chart"`
			Version   string           `json:"version"`
			Installed *bool            `json:"installed"`
			Needs     []string         `json:"needs"`
			Values    []map[string]any `json:"values"`
			Set       []struct {
				Name  string `json:"name"`
				Value any    `json:"value"`
			} `json:"set"`
			Hooks []struct {
				Events  []string `json:"events"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"releases"`
	}
	readYAML(t, filepath.Join("..", "helmfile.yaml"), &helmfile)
	repositories := map[string]string{}
	for _, repository := range helmfile.Repositories {
		repositories[repository.Name] = repository.URL
	}
	releaseChart := map[string]string{} // release name -> chart name, to resolve `needs`
	for _, release := range helmfile.Releases {
		if repo, chart, ok := strings.Cut(release.Chart, "/"); ok && repositories[repo] != "" {
			releaseChart[release.Name] = chart
		}
	}
	releases := map[string]platformRelease{}
	for _, release := range helmfile.Releases {
		repo, chart, ok := strings.Cut(release.Chart, "/")
		if !ok || repositories[repo] == "" {
			continue // ./chart: the operator itself, which a leaf gets through OramaOperator
		}
		if release.Installed != nil && !*release.Installed {
			continue
		}
		if release.Version == "" {
			t.Errorf("helmfile.yaml: release %s has no version. A leaf's ClusterHelmRelease is always pinned, so an unpinned release here cannot be the same as a leaf's", release.Name)
		}
		entry := platformRelease{Release: release.Name, Repository: repositories[repo], Chart: chart, Version: release.Version, Namespace: release.Namespace, Values: map[string]any{}}
		for _, values := range release.Values {
			mergeValues(entry.Values, values)
		}
		for _, set := range release.Set {
			setValue(entry.Values, set.Name, set.Value)
		}
		for _, need := range release.Needs {
			name := need[strings.LastIndex(need, "/")+1:]
			entry.DependsOn = append(entry.DependsOn, releaseChart[name])
		}
		for _, hook := range release.Hooks {
			for _, arg := range hook.Args {
				if strings.HasPrefix(arg, "https://") {
					entry.CRDURLs = append(entry.CRDURLs, arg)
				}
			}
		}
		releases[chart] = entry
	}
	return releases
}

// leafPlatform reads one managed cluster's ClusterHelmRelease resources, by chart name.
func leafPlatform(t *testing.T, path string) map[string]platformRelease {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type resource struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Namespace   string `json:"namespace"`
			ReleaseName string `json:"releaseName"`
			Chart       struct {
				Repository string `json:"repository"`
				Name       string `json:"name"`
				Version    string `json:"version"`
			} `json:"chart"`
			Values    map[string]any `json:"values"`
			DependsOn []string       `json:"dependsOn"`
			CRDURLs   []string       `json:"crdURLs"`
		} `json:"spec"`
	}
	var resources []resource
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var r resource
		if err := yaml.Unmarshal(doc, &r); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if r.Kind == "ClusterHelmRelease" {
			resources = append(resources, r)
		}
	}
	chartOf := map[string]string{} // resource name -> chart name, to resolve dependsOn
	for _, r := range resources {
		chartOf[r.Metadata.Name] = r.Spec.Chart.Name
	}
	releases := map[string]platformRelease{}
	for _, r := range resources {
		entry := platformRelease{
			Release:    r.Spec.ReleaseName,
			Repository: r.Spec.Chart.Repository, Chart: r.Spec.Chart.Name, Version: r.Spec.Chart.Version,
			Namespace: r.Spec.Namespace, Values: r.Spec.Values, CRDURLs: r.Spec.CRDURLs,
		}
		if entry.Values == nil {
			entry.Values = map[string]any{}
		}
		for _, name := range r.Spec.DependsOn {
			entry.DependsOn = append(entry.DependsOn, chartOf[name])
		}
		releases[entry.Chart] = entry
	}
	return releases
}

func normalized(release platformRelease) platformRelease {
	release.Repository = strings.TrimSuffix(release.Repository, "/")
	sort.Strings(release.DependsOn)
	sort.Strings(release.CRDURLs)
	if len(release.DependsOn) == 0 {
		release.DependsOn = nil
	}
	if len(release.CRDURLs) == 0 {
		release.CRDURLs = nil
	}
	// Through JSON, so a number written 1 and one written 1.0 compare equal.
	raw, _ := json.Marshal(release.Values)
	release.Values = map[string]any{}
	_ = json.Unmarshal(raw, &release.Values)
	return release
}

func TestHelmfileAndEveryLeafInstallTheSamePlatform(t *testing.T) {
	management := helmfilePlatform(t)
	if len(management) == 0 {
		t.Fatal("found no third-party releases in helmfile.yaml; the format this test reads changed")
	}
	leaves, err := filepath.Glob(filepath.Join("..", "config", "clusters", "*", "platform.yaml"))
	if err != nil || len(leaves) == 0 {
		t.Fatalf("found no config/clusters/*/platform.yaml: %v", err)
	}
	for _, path := range leaves {
		cluster := filepath.Base(filepath.Dir(path))
		leaf := leafPlatform(t, path)
		for chart, want := range management {
			got, ok := leaf[chart]
			if !ok {
				t.Errorf("%s: helmfile.yaml installs %s on the management cluster, and %s has no ClusterHelmRelease for it", cluster, chart, path)
				continue
			}
			if want, got := normalized(want), normalized(got); !reflect.DeepEqual(want, got) {
				wantJSON, _ := json.MarshalIndent(want, "  ", "  ")
				gotJSON, _ := json.MarshalIndent(got, "  ", "  ")
				t.Errorf("%s: %s differs between the two installation paths.\n  helmfile.yaml (management cluster):\n  %s\n  %s (leaf):\n  %s\n"+
					"Change both in the same pull request.", cluster, chart, wantJSON, path, gotJSON)
			}
		}
		for chart := range leaf {
			if _, ok := management[chart]; !ok {
				t.Errorf("%s: %s installs %s on the leaf, and helmfile.yaml does not install it on the management cluster", cluster, path, chart)
			}
		}
	}
}

// Every cluster runs the same operator version: the OramaOperator resources must agree with each
// other. (The management cluster's own version is whatever was last rolled onto it from the
// helmfile path; it is not in a file yet, which docs/infrastructure/README.md records as a gap.)
func TestEveryLeafRunsTheSameOperatorVersion(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "config", "clusters", "*", "operator.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("found no config/clusters/*/operator.yaml: %v", err)
	}
	versions := map[string][]string{}
	for _, path := range files {
		var operator struct {
			Spec struct {
				Version         string `json:"version"`
				ImageRepository string `json:"imageRepository"`
				Values          any    `json:"values"`
			} `json:"spec"`
		}
		readYAML(t, path, &operator)
		key := operator.Spec.ImageRepository + ":" + operator.Spec.Version
		versions[key] = append(versions[key], filepath.Base(filepath.Dir(path)))
		if operator.Spec.Values != nil {
			t.Errorf("%s sets spec.values: an operator must not be configured differently on one cluster (CLAUDE.md rule 11)", path)
		}
	}
	if len(versions) != 1 {
		t.Errorf("the managed clusters do not all run the same operator: %v", versions)
	}
}

// operator#719: on every cluster, ingress-nginx takes the client address from CF-Connecting-IP only
// for connections from Cloudflare's published ranges, so a sender that reaches the ingress directly
// cannot choose the address lake counts (lake#838).
func TestIngressTrustsCloudflaresClientAddressOnlyFromCloudflare(t *testing.T) {
	check := func(where string, release platformRelease) {
		controller, _ := release.Values["controller"].(map[string]any)
		config, _ := controller["config"].(map[string]any)
		if config["use-forwarded-headers"] != "true" || config["forwarded-for-header"] != "CF-Connecting-IP" {
			t.Errorf("%s: ingress-nginx config %v does not take the address from CF-Connecting-IP", where, config)
		}
		cidrs, _ := config["proxy-real-ip-cidr"].(string)
		for _, want := range []string{"173.245.48.0/20", "104.16.0.0/13", "172.64.0.0/13", "2606:4700::/32"} {
			if !strings.Contains(cidrs, want) {
				t.Errorf("%s: proxy-real-ip-cidr lacks Cloudflare's %s", where, want)
			}
		}
		for _, private := range []string{"0.0.0.0/0", "10.0.0.0/8", "::/0"} {
			if strings.Contains(cidrs, private) {
				t.Errorf("%s: proxy-real-ip-cidr trusts %s, not only Cloudflare", where, private)
			}
		}
	}
	check("helmfile.yaml", helmfilePlatform(t)["ingress-nginx"])
	paths, err := filepath.Glob("../config/clusters/*/platform.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no leaf platform files (%v)", err)
	}
	for _, path := range paths {
		check(path, leafPlatform(t, path)["ingress-nginx"])
	}
}
