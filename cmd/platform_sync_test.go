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
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oramasearch/orama-operator/internal/platformcharts"
)

// THE RULE (CLAUDE.md, "Two installation paths, one result"): the management cluster is installed
// from helmfile.yaml, and every leaf by the management cluster's operator from the
// ClusterHelmRelease resources in config/clusters/<cluster>/platform.yaml. The two paths must
// install the same charts at the same versions, in the same namespaces, with the same values, so
// that a management cluster and a leaf are effectively the same cluster. Steve, 2026-10-13: "both
// the helmfile and the operator need to be kept in sync".
//
// This test is what keeps them in step. It fails when a chart, a version, a pin, a namespace, a
// value, a dependency or a separately-shipped CRD manifest is in one and not the other.
//
// helmfile cannot check a digest, so its path installs local archives staged by hack/stagecharts
// against the pins in platform-charts.yaml (orama-operator#808). Its releases are read through
// those pins: the archive a release names is the chart, version and digest it installs.

// platformRelease is one chart release, described the same way whichever path it came from.
type platformRelease struct {
	// Release is the Helm release name. Two paths that install the same chart under different
	// release names would leave a cluster with two copies of it (operator#753).
	Release    string
	Repository string
	Chart      string
	Version    string
	SHA256     string
	Namespace  string
	Values     map[string]any
	DependsOn  []string // chart names
	CRDURLs    []string
	CRDSHA256  map[string]string
}

// The directory hack/stagecharts stages into, as helmfile.yaml names it.
const stagedDir = "./.platform-charts/"

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

// helmfilePlatform reads the third-party releases of helmfile.yaml, by chart name, each as the
// pinned archive it installs.
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
	if len(helmfile.Repositories) > 0 {
		t.Errorf("helmfile.yaml lists chart repositories %v: helmfile would download charts whose bytes nothing checks. Pin the chart in platform-charts.yaml and install its staged archive", helmfile.Repositories)
	}
	pins, err := platformcharts.Load(filepath.Join("..", "platform-charts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	archives := map[string]platformcharts.Chart{} // staged archive path -> its pin
	crdFiles := map[string]platformcharts.CRD{}   // staged CRD manifest path -> its pin
	for _, chart := range pins.Charts {
		archives[stagedDir+chart.Archive()] = chart
		for _, crd := range chart.CRDs {
			crdFiles[stagedDir+chart.CRDFile(crd)] = crd
		}
	}
	releaseChart := map[string]string{} // release name -> chart name, to resolve `needs`
	for _, release := range helmfile.Releases {
		if pin, ok := archives[release.Chart]; ok {
			releaseChart[release.Name] = pin.Name
		}
	}
	installed := map[string]bool{} // pinned archives and CRD manifests some release installs
	releases := map[string]platformRelease{}
	for _, release := range helmfile.Releases {
		if release.Chart == "./chart" {
			continue // the operator itself, which a leaf gets through OramaOperator
		}
		pin, ok := archives[release.Chart]
		if !ok {
			t.Errorf("helmfile.yaml: release %s installs %s, which is not an archive pinned in platform-charts.yaml and staged in %s", release.Name, release.Chart, stagedDir)
			continue
		}
		installed[release.Chart] = true
		if release.Installed != nil && !*release.Installed {
			continue
		}
		if release.Version != "" {
			t.Errorf("helmfile.yaml: release %s sets version %s; the staged archive it names, and its pin, say which version it installs", release.Name, release.Version)
		}
		entry := platformRelease{Release: release.Name, Repository: pin.Repository, Chart: pin.Name, Version: pin.Version, SHA256: pin.SHA256, Namespace: release.Namespace, Values: map[string]any{}}
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
				switch {
				case strings.HasPrefix(arg, "https://"), strings.HasPrefix(arg, "http://"):
					t.Errorf("helmfile.yaml: release %s's hook reads %s, whose bytes nothing checks. Pin it in platform-charts.yaml and apply the staged file", release.Name, arg)
				case strings.HasPrefix(arg, stagedDir):
					crd, ok := crdFiles[arg]
					if !ok {
						t.Errorf("helmfile.yaml: release %s's hook applies %s, which platform-charts.yaml does not pin", release.Name, arg)
						continue
					}
					installed[arg] = true
					entry.CRDURLs = append(entry.CRDURLs, crd.URL)
					if entry.CRDSHA256 == nil {
						entry.CRDSHA256 = map[string]string{}
					}
					entry.CRDSHA256[crd.URL] = crd.SHA256
				}
			}
		}
		releases[pin.Name] = entry
	}
	for file := range archives {
		if !installed[file] {
			t.Errorf("platform-charts.yaml pins %s, which no release in helmfile.yaml installs", file)
		}
	}
	for file := range crdFiles {
		if !installed[file] {
			t.Errorf("platform-charts.yaml pins the CRD manifest staged as %s, which no hook in helmfile.yaml applies", file)
		}
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
				SHA256     string `json:"sha256"`
			} `json:"chart"`
			Values    map[string]any    `json:"values"`
			DependsOn []string          `json:"dependsOn"`
			CRDURLs   []string          `json:"crdURLs"`
			CRDSHA256 map[string]string `json:"crdSHA256"`
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
			Repository: r.Spec.Chart.Repository, Chart: r.Spec.Chart.Name, Version: r.Spec.Chart.Version, SHA256: r.Spec.Chart.SHA256,
			Namespace: r.Spec.Namespace, Values: r.Spec.Values, CRDURLs: r.Spec.CRDURLs, CRDSHA256: r.Spec.CRDSHA256,
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
	if len(release.CRDSHA256) == 0 {
		release.CRDSHA256 = nil
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

// Every cluster runs the same operator: the OramaOperator resources must agree with each other on
// the version and the digests it is installed by, and the chart's default image, which helmfile's
// path installs on the management cluster, is that version at that digest. (A management cluster
// rolled by hand with another tag is not in a file yet, which docs/infrastructure/README.md
// records as a gap.)
func TestEveryClusterRunsTheSameOperatorVersion(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "config", "clusters", "*", "operator.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("found no config/clusters/*/operator.yaml: %v", err)
	}
	type operatorSpec struct {
		Version         string `json:"version"`
		ImageDigest     string `json:"imageDigest"`
		ChartSHA256     string `json:"chartSHA256"`
		ImageRepository string `json:"imageRepository"`
		Values          any    `json:"values"`
	}
	versions := map[operatorSpec][]string{}
	var leaf operatorSpec
	for _, path := range files {
		var operator struct {
			Spec operatorSpec `json:"spec"`
		}
		readYAML(t, path, &operator)
		if operator.Spec.Values != nil {
			t.Errorf("%s sets spec.values: an operator must not be configured differently on one cluster (CLAUDE.md rule 11)", path)
			operator.Spec.Values = nil
		}
		for field, digest := range map[string]string{"imageDigest": operator.Spec.ImageDigest, "chartSHA256": operator.Spec.ChartSHA256} {
			if !digestPattern.MatchString(digest) {
				t.Errorf("%s: spec.%s %q is not a sha256 digest; the operator is installed only by its pins (orama-operator#808)", path, field, digest)
			}
		}
		leaf = operator.Spec
		versions[operator.Spec] = append(versions[operator.Spec], filepath.Base(filepath.Dir(path)))
	}
	if len(versions) != 1 {
		t.Errorf("the managed clusters do not all run the same operator: %v", versions)
	}

	var chartValues struct {
		Operator struct {
			Image struct{ Repository, Tag string } `json:"image"`
		} `json:"operator"`
	}
	readYAML(t, filepath.Join("..", "chart", "values.yaml"), &chartValues)
	image := chartValues.Operator.Image
	if want := leaf.Version + "@" + leaf.ImageDigest; image.Repository != leaf.ImageRepository || image.Tag != want {
		t.Errorf("chart/values.yaml installs %s:%s on the management cluster, and the leaves run %s:%s. Change both together",
			image.Repository, image.Tag, leaf.ImageRepository, want)
	}
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// operator#719: on every cluster, ingress-nginx takes the client address from CF-Connecting-IP only
// for connections from Cloudflare's published ranges, so a sender that reaches the ingress directly
// cannot choose the address lake counts (lake#838).
func TestIngressTrustsCloudflaresClientAddressOnlyFromCloudflare(t *testing.T) {
	check := func(where string, release platformRelease) {
		controller, _ := release.Values["controller"].(map[string]any)
		config, _ := controller["config"].(map[string]any)
		if config["enable-real-ip"] != "true" || config["forwarded-for-header"] != "CF-Connecting-IP" {
			t.Errorf("%s: ingress-nginx config %v does not take the address from CF-Connecting-IP", where, config)
		}
		// use-forwarded-headers would also pass a sender's own X-Forwarded-Host, -Proto and -Port to
		// every backend; only the client address is taken from Cloudflare.
		if _, set := config["use-forwarded-headers"]; set {
			t.Errorf("%s: ingress-nginx sets use-forwarded-headers, which trusts X-Forwarded-* from any sender", where)
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
