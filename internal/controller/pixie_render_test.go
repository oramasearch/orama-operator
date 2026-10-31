/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// testPixieRelease names every image by a digest derived from its key.
func testPixieRelease() oramav1alpha1.PixieRelease {
	images := map[string]oramav1alpha1.ImageDigestReference{}
	for _, key := range pixieImageKeys {
		images[key] = oramav1alpha1.ImageDigestReference(fmt.Sprintf("registry.example/oramasearch/pixie-%s@sha256:%x", key, sha256.Sum256([]byte(key))))
	}
	// CloudNativePG reads the version from the tag.
	images["postgres"] = oramav1alpha1.ImageDigestReference(fmt.Sprintf("registry.example/oramasearch/pixie-postgres:16.15@sha256:%x", sha256.Sum256([]byte("postgres"))))
	return oramav1alpha1.PixieRelease{Commit: pixieManifestCommit, VizierVersion: "0.14.16-orama.1", CLIVersion: "0.8.9-orama.1", Images: images}
}

func pixieReleasePtr(release oramav1alpha1.PixieRelease) *oramav1alpha1.PixieRelease { return &release }

func testPixieVizier() *oramav1alpha1.PixieVizier {
	return &oramav1alpha1.PixieVizier{
		ObjectMeta: metav1.ObjectMeta{Name: "vizier", Namespace: oramav1alpha1.PixieVizierNamespace},
		Spec: oramav1alpha1.PixieVizierSpec{
			Release:             pixieReleasePtr(testPixieRelease()),
			Agent:               "prod2",
			Purpose:             "production",
			ClusterName:         "orama-production-cluster-02",
			CloudAddress:        "peer-pixie-cloud.pl.svc:51600",
			CloudCASecretName:   "pixie-cloud-ca",
			DeployKeySecretName: vizierDeploySecret,
			PullSecretName:      "pixie-pull",
			NodeMemory:          resource.MustParse("2Gi"),
		},
	}
}

func testPixieCloud() *oramav1alpha1.PixieCloud {
	return &oramav1alpha1.PixieCloud{
		ObjectMeta: metav1.ObjectMeta{Name: "hub", Namespace: oramav1alpha1.PixieCloudNamespace},
		Spec: oramav1alpha1.PixieCloudSpec{
			Release:        testPixieRelease(),
			Hub:            "production",
			Purpose:        "production",
			Domain:         "pixie.support.example",
			Organization:   "orama",
			AdminEmail:     "ops@support.example",
			PullSecretName: "pixie-pull",
		},
	}
}

func TestPixieManifestCommitMatchesAssets(t *testing.T) {
	data, err := os.ReadFile("assets/pixie/COMMIT")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != pixieManifestCommit {
		t.Fatalf("assets/pixie/COMMIT is %s, pixieManifestCommit is %s: re-run hack/fetch-pixie-manifests.sh and move both together", got, pixieManifestCommit)
	}
}

// Every image a vendored bundle names is one a release replaces, so a re-vendored commit with a
// new image fails here instead of pulling from a public registry.
func TestPixieBundlesUseOnlyMappedImages(t *testing.T) {
	used := map[string]bool{}
	for _, asset := range []string{"vizier.yaml", "vizier-nats.yaml", "cloud.yaml", "cloud-nats.yaml"} {
		objects, err := decodePixieAsset(asset)
		if err != nil {
			t.Fatal(err)
		}
		dropped := map[pixieObjectKey]bool{}
		for _, key := range append(append([]pixieObjectKey{}, vizierDropped...), pixieCloudDropped...) {
			dropped[key] = true
		}
		for _, object := range objects {
			spec, _ := podSpecOf(object)
			if spec == nil || dropped[keyOf(object)] {
				continue
			}
			for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
				key, ok := pixieUpstreamImages[upstreamImageRepository(c.Image)]
				if !ok {
					t.Errorf("%s %s container %s runs %s, which no release key maps", asset, object.GetName(), c.Name, c.Image)
				}
				used[key] = true
			}
		}
	}
	// The release keys the operator adds itself, beyond the vendored manifests.
	for _, own := range []string{"opensearch", "script-bundle", "postgres"} {
		used[own] = true
	}
	for _, key := range pixieImageKeys {
		if !used[key] {
			t.Errorf("release key %s replaces no image", key)
		}
	}
}

func TestPixieReleaseProblems(t *testing.T) {
	release := testPixieRelease()
	if problems := pixieReleaseProblems(&release); len(problems) != 0 {
		t.Fatalf("a complete release has problems: %v", problems)
	}
	cases := map[string]func(*oramav1alpha1.PixieRelease){
		"another commit":   func(r *oramav1alpha1.PixieRelease) { r.Commit = strings.Repeat("a", 40) },
		"missing image":    func(r *oramav1alpha1.PixieRelease) { delete(r.Images, "vizier-pem") },
		"unknown image":    func(r *oramav1alpha1.PixieRelease) { r.Images["grafana"] = r.Images["curl"] },
		"tag, not digest":  func(r *oramav1alpha1.PixieRelease) { r.Images["kratos"] = "oryd/kratos:v0.10.1" },
		"truncated digest": func(r *oramav1alpha1.PixieRelease) { r.Images["kratos"] = "oryd/kratos@sha256:abc" },
		"postgres without a tag": func(r *oramav1alpha1.PixieRelease) {
			r.Images["postgres"] = oramav1alpha1.ImageDigestReference("registry.example/pixie-postgres@sha256:" + strings.Repeat("a", 64))
		},
	}
	for name, mutate := range cases {
		release := testPixieRelease()
		mutate(&release)
		if problems := pixieReleaseProblems(&release); len(problems) == 0 {
			t.Errorf("%s: no problem reported", name)
		}
	}
}

// The committed release (#455) must be one this operator installs: its commit is the vendored one
// and it names every key by digest. Moving the operator to another Pixie commit fails here until
// the release is rebuilt.
func TestCommittedPixieReleaseIsInstallable(t *testing.T) {
	raw, err := os.ReadFile("../../config/clusters/observability-provider.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var provider oramav1alpha1.ObservabilityProvider
	if err := sigsyaml.UnmarshalStrict(raw, &provider); err != nil {
		t.Fatal(err)
	}
	if problems := pixieReleaseProblems(provider.Spec.Pixie); len(problems) != 0 {
		t.Fatalf("the committed release cannot be installed: %v", problems)
	}
}

// forbiddenImageFragments are image names from distributions this operator must never ship:
// Grafana's AGPL family and Elastic's non-permissive builds, and the public registries Pixie's
// manifests pull from (everything comes from the release, by digest).
var forbiddenImageFragments = []string{"grafana", "loki", "mimir", "tempo", "elastic", "kibana", "eck-operator", "gcr.io/", "ghcr.io/pixie-io", "oryd/", "envoyproxy/"}

func assertPixiePods(t *testing.T, objects []client.Object, release oramav1alpha1.PixieRelease) {
	t.Helper()
	allowed := map[string]bool{}
	for _, ref := range release.Images {
		allowed[string(ref)] = true
	}
	for _, object := range objects {
		spec, _ := podSpecOf(object)
		if spec == nil {
			continue
		}
		if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "pixie-pull" {
			t.Errorf("%s pulls with %v, not the cluster's own pull Secret", object.GetName(), spec.ImagePullSecrets)
		}
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			if !allowed[c.Image] {
				t.Errorf("%s/%s runs %s, not a release image", object.GetName(), c.Name, c.Image)
			}
			for _, fragment := range forbiddenImageFragments {
				if strings.Contains(strings.TrimPrefix(c.Image, "registry.example/oramasearch/pixie-"), fragment) && !strings.HasPrefix(c.Image, "registry.example/") {
					t.Errorf("%s/%s runs forbidden image %s", object.GetName(), c.Name, c.Image)
				}
			}
			if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() {
				t.Errorf("%s/%s has no memory and CPU requests and limits", object.GetName(), c.Name)
			}
			for _, env := range c.Env {
				if env.Name == "PL_SENTRY_DSN" {
					t.Errorf("%s/%s still reports to Sentry", object.GetName(), c.Name)
				}
			}
		}
	}
}

func TestRenderVizier(t *testing.T) {
	vz := testPixieVizier()
	objects, err := renderVizier(vizierRender{Vizier: vz, CertsHash: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	assertPixiePods(t, objects, *vz.Spec.Release)
	byKey := map[pixieObjectKey]client.Object{}
	for _, object := range objects {
		byKey[keyOf(object)] = object
		if object.GetLabels()[pixieManagedLabel] != pixieManagedVizier {
			t.Errorf("%s %s is not labelled as the operator's Vizier", kindOf(object), object.GetName())
		}
		if _, clusterScoped := object.(*rbacv1.ClusterRole); !clusterScoped && kindOf(object) != "ClusterRoleBinding" && object.GetNamespace() != "pl" {
			t.Errorf("%s %s is in namespace %q", kindOf(object), object.GetName(), object.GetNamespace())
		}
	}
	for _, dropped := range vizierDropped {
		if _, found := byKey[dropped]; found {
			t.Errorf("%s %s is rendered, but the operator never applies it", dropped.Kind, dropped.Name)
		}
	}
	for _, object := range objects {
		if _, isJob := object.(*batchv1.Job); isJob {
			t.Errorf("Vizier renders Job %s; certificates are the operator's", object.GetName())
		}
	}
	clusterConfig := byKey[pixieObjectKey{"ConfigMap", vizierClusterConfig}].(*corev1.ConfigMap)
	if clusterConfig.Data["PL_DISABLE_AUTO_UPDATE"] != "true" {
		t.Errorf("auto-update is %q, want true", clusterConfig.Data["PL_DISABLE_AUTO_UPDATE"])
	}
	cloudConfig := byKey[pixieObjectKey{"ConfigMap", vizierCloudConfig}].(*corev1.ConfigMap)
	if cloudConfig.Data["PL_CLOUD_ADDR"] != vz.Spec.CloudAddress || strings.Contains(cloudConfig.Data["PL_CLOUD_ADDR"], ".svc.cluster.local") {
		t.Errorf("PL_CLOUD_ADDR is %q: Pixie skips certificate verification for .svc.cluster.local", cloudConfig.Data["PL_CLOUD_ADDR"])
	}
	connector := byKey[pixieObjectKey{"Deployment", "vizier-cloud-connector"}].(*appsv1.Deployment)
	app := containerNamed(&connector.Spec.Template.Spec, "app")
	envs := map[string]corev1.EnvVar{}
	for _, env := range app.Env {
		envs[env.Name] = env
	}
	if envs["SSL_CERT_FILE"].Value != vizierCloudCAPath+"/ca.crt" || envs["PL_DISABLE_AUTO_UPDATE"].Value != "true" {
		t.Errorf("the cloud connector does not verify the hub with its CA, or may update itself: %v", envs)
	}
	if ref := envs["PL_DEPLOY_KEY"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != vizierDeploySecret {
		t.Errorf("the deploy key does not come from %s", vizierDeploySecret)
	}
	if connector.Spec.Template.Annotations[pixieCertsAnnotation] != "abc" {
		t.Errorf("a certificate change does not roll the cloud connector")
	}
	pem := byKey[pixieObjectKey{"DaemonSet", "vizier-pem"}].(*appsv1.DaemonSet)
	agent := containerNamed(&pem.Spec.Template.Spec, vizierPEMContainerName)
	if got := agent.Resources.Limits.Memory().String(); got != "2Gi" {
		t.Errorf("node agent memory limit %s, want spec.nodeMemory 2Gi", got)
	}
	for _, env := range agent.Env {
		if env.Name == "PL_TABLE_STORE_DATA_LIMIT_MB" && env.Value != "1228" {
			t.Errorf("table store %s MiB, want 60%% of 2Gi", env.Value)
		}
	}
	broker := byKey[pixieObjectKey{"Deployment", "vizier-query-broker"}].(*appsv1.Deployment)
	for _, volume := range broker.Spec.Template.Spec.Volumes {
		if volume.Name == "envoy-yaml" {
			t.Errorf("the query broker keeps an unmounted ConfigMap volume the kubelet would wait for")
		}
	}
}

func TestRenderVizierRefusesTinyNodeMemory(t *testing.T) {
	vz := testPixieVizier()
	vz.Spec.NodeMemory = resource.MustParse("512Mi")
	if _, err := renderVizier(vizierRender{Vizier: vz}); err == nil {
		t.Fatal("a node agent below 1Gi was rendered")
	}
}

func TestRenderPixieCloud(t *testing.T) {
	cloud := testPixieCloud()
	objects, err := renderPixieCloud(pixieCloudRender{Cloud: cloud, CertsHash: "abc", DatabaseHost: pixieDBHost()})
	if err != nil {
		t.Fatal(err)
	}
	assertPixiePods(t, objects, cloud.Spec.Release)
	byKey := map[pixieObjectKey]client.Object{}
	for _, object := range objects {
		byKey[keyOf(object)] = object
	}
	if _, found := byKey[pixieObjectKey{"Deployment", "metrics-server"}]; found {
		t.Error("the BigQuery metrics exporter is rendered")
	}
	for _, object := range objects {
		spec, _ := podSpecOf(object)
		if spec == nil {
			continue
		}
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && (strings.Contains(volume.Secret.SecretName, "elastic") || volume.Secret.SecretName == "artifact-access-sa" || volume.Secret.SecretName == "bq-access-sa") {
				t.Errorf("%s mounts %s", object.GetName(), volume.Secret.SecretName)
			}
		}
		for _, c := range spec.Containers {
			for _, env := range c.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					name := env.ValueFrom.SecretKeyRef.Name
					if name == "pl-db-secrets" || strings.Contains(name, "elastic") {
						t.Errorf("%s/%s reads %s from %s", object.GetName(), c.Name, env.Name, name)
					}
				}
			}
		}
		if c := containerNamed(spec, "admin-create-if-not-exists"); c != nil {
			t.Error("the upstream admin container with password admin is rendered")
		}
	}
	// One database and role per service; services without data get no credentials.
	vzmgr := containerNamed(&byKey[pixieObjectKey{"Deployment", "vzmgr-server"}].(*appsv1.Deployment).Spec.Template.Spec, "vzmgr-server")
	envs := map[string]corev1.EnvVar{}
	for _, env := range vzmgr.Env {
		envs[env.Name] = env
	}
	if envs["PL_POSTGRES_DB"].Value != "vzmgr" || envs["PL_POSTGRES_USERNAME"].ValueFrom.SecretKeyRef.Name != pixieDatabaseSecret("vzmgr") ||
		envs["PL_DATABASE_KEY"].ValueFrom.SecretKeyRef.Name != pixieDatabaseSecret("vzmgr") {
		t.Errorf("vzmgr does not use its own database, role and key: %v", envs)
	}
	vzconn := containerNamed(&byKey[pixieObjectKey{"Deployment", "vzconn-server"}].(*appsv1.Deployment).Spec.Template.Spec, "vzconn-server")
	for _, env := range vzconn.Env {
		if strings.HasPrefix(env.Name, "PL_POSTGRES_") {
			t.Errorf("vzconn, which opens no database, gets %s", env.Name)
		}
	}
	indexer := byKey[pixieObjectKey{"ConfigMap", "pl-indexer-config"}].(*corev1.ConfigMap)
	if indexer.Data["PL_MD_MANUAL_INDEX_MANAGEMENT"] != "true" {
		t.Error("the indexer would call Elastic's ILM API")
	}
	artifacts := byKey[pixieObjectKey{"ConfigMap", "pl-artifact-config"}].(*corev1.ConfigMap)
	bundles := byKey[pixieObjectKey{"ConfigMap", "pl-script-bundles-config"}].(*corev1.ConfigMap)
	for _, value := range []string{artifacts.Data["PL_ARTIFACT_MANIFEST_URL"], bundles.Data["SCRIPT_BUNDLE_URLS"]} {
		if strings.Contains(value, "px.dev") || strings.Contains(value, "googleapis") {
			t.Errorf("the hub still fetches %s", value)
		}
	}
	scriptmgr := byKey[pixieObjectKey{"Deployment", "scriptmgr-server"}].(*appsv1.Deployment)
	for _, env := range containerNamed(&scriptmgr.Spec.Template.Spec, "scriptmgr-server").Env {
		// Google's storage client prepends http:// itself; a scheme here breaks every read.
		if env.Name == "STORAGE_EMULATOR_HOST" && env.Value != "pixie-scripts.plc.svc:8080" {
			t.Errorf("STORAGE_EMULATOR_HOST is %q, want host:port of the bundle server", env.Value)
		}
	}
	if scriptmgr.Spec.Template.Annotations["orama.cloud/pixie-script-bundle"] != string(cloud.Spec.Release.Images["script-bundle"]) {
		t.Error("a new script bundle does not roll the script manager, which reads it only at start")
	}
	nginx := byKey[pixieObjectKey{"ConfigMap", pixieProxyNginxConfig}].(*corev1.ConfigMap)
	if !strings.Contains(nginx.Data["pixie_api.conf"], "location /pxl_scripts/") {
		t.Error("the proxy does not serve the script bundle")
	}
	nats := byKey[pixieObjectKey{"StatefulSet", "pl-nats"}].(*appsv1.StatefulSet)
	if got := nats.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); got != "10Gi" {
		t.Errorf("JetStream volumes are %s, want spec.storage.messaging's default 10Gi", got)
	}
	natsConf := byKey[pixieObjectKey{"ConfigMap", "nats-config"}].(*corev1.ConfigMap).Data["nats.conf"]
	if !strings.Contains(natsConf, "max_file_store: 9663676416") || !strings.Contains(natsConf, "max_memory_store: 134217728") {
		t.Errorf("JetStream is not capped below its volume and memory limit:\n%s", natsConf)
	}
	for _, object := range objects {
		if _, isJob := object.(*batchv1.Job); isJob {
			t.Errorf("the hub renders Job %s; the plugin loader needs GitHub", object.GetName())
		}
	}
	// Rendering twice gives the same objects: reconciliation is deterministic.
	again, err := renderPixieCloud(pixieCloudRender{Cloud: cloud, CertsHash: "abc", DatabaseHost: pixieDBHost()})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(objects)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Error("two renders of the same PixieCloud differ")
	}
}

// The hub's proxy certificate is what a Vizier verifies: issued by the hub's CA, for exactly the
// addresses a Vizier may dial. A TLS handshake with the same settings Pixie's cloud connector ends
// up with (Go's root file set to the hub CA, the server name taken from the dial address) succeeds
// for those addresses and fails for any other name or any other CA.
func TestHubCertificateIsVerifiedForTheDialedAddress(t *testing.T) {
	now := time.Now()
	hubCA, err := newTunnelCA("Pixie Cloud CA test", now)
	if err != nil {
		t.Fatal(err)
	}
	otherCA, err := newTunnelCA("another CA", now)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := issueTunnelLeaf(hubCA, tunnelLeafRequest{CommonName: "pixie.example", DNSNames: pixieProxyDNSNames("pixie.example"), Server: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _ = conn.(*tls.Conn).Handshake(); _ = conn.Close() }()
		}
	}()
	dial := func(serverName string, ca tunnelCA) error {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca.CertPEM)
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12})
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	for address := range vizierCloudAddresses {
		host, _, _ := net.SplitHostPort(address)
		if err := dial(host, hubCA); err != nil {
			t.Errorf("a Vizier dialing %s does not verify the hub: %v", address, err)
		}
		if err := dial(host, otherCA); err == nil {
			t.Errorf("a certificate from another CA is accepted for %s", address)
		}
		if err := checkVizierCloudAddress(address); err != nil {
			t.Errorf("rendering refuses %s: %v", address, err)
		}
	}
	if err := dial("vzconn-service.other.svc", hubCA); err == nil {
		t.Error("the hub's certificate is accepted for a name it was not issued for")
	}
	for _, refused := range []string{"vzconn-service.plc.svc.cluster.local:51600", "pixie.example:51600", "peer-pixie-cloud.pl.svc:443"} {
		if err := checkVizierCloudAddress(refused); err == nil {
			t.Errorf("cloudAddress %s is rendered", refused)
		}
	}
}

// hack/observability-images.yaml lists exactly the keys a release must name, so the inventory #455
// builds and mirrors from is the one the operator renders.
func TestPixieInventoryMatchesReleaseKeys(t *testing.T) {
	data, err := os.ReadFile("../../hack/observability-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Pixie []struct {
			Key string `json:"key"`
		} `json:"pixie"`
	}
	if err := sigsyaml.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, entry := range inventory.Pixie {
		keys = append(keys, entry.Key)
	}
	want := append([]string(nil), pixieImageKeys...)
	sort.Strings(keys)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("inventory keys %v, release keys %v", keys, want)
	}
}
