/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// The Helm release the operator is installed as, on every cluster.
	leafOperatorRelease = "orama-operator"
	// The label the chart puts on the operator's Deployment and pods (.Chart.Name).
	leafOperatorNameLabel = "app.kubernetes.io/name"
	leafOperatorNameValue = "orama-operator"
	// The field owner of what this controller applies directly on a leaf: CRDs, the namespace
	// and the pull secret.
	leafFieldOwner = "orama-operator-management"

	leafAPITimeout = 30 * time.Second
)

// chartSource says where a chart is. Repository is either a classic chart repository
// (https://...), with Name the chart in it, or an OCI repository holding the chart
// (oci://host/path, or host/path), with Name unused. Version is always pinned.
type chartSource struct {
	Repository string
	Name       string
	Version    string
	User       string
	Pass       string
}

func (c chartSource) oci() bool {
	return !strings.HasPrefix(c.Repository, "https://") && !strings.HasPrefix(c.Repository, "http://")
}

func (c chartSource) String() string {
	if c.oci() {
		return strings.TrimPrefix(c.Repository, "oci://") + ":" + c.Version
	}
	return c.Repository + " " + c.Name + " " + c.Version
}

// helmReleaseRequest is one chart release a managed cluster should have.
type helmReleaseRequest struct {
	Release   string
	Namespace string
	Chart     chartSource
	Values    map[string]any
	// RequiredCRDs are checked on the cluster before the chart is installed or upgraded.
	RequiredCRDs []string
	// CRDURLs are manifests of CRDs a chart ships apart from itself. They are applied before the
	// chart, on every install and upgrade.
	CRDURLs []string
}

// helmReleaseState is what the cluster has.
type helmReleaseState struct {
	ChartName    string
	ChartVersion string
	AppVersion   string
	Revision     int
	Status       string // Helm's word: deployed, failed, pending-upgrade
	Values       map[string]any
	// Workloads counts the release's Deployments, StatefulSets and DaemonSets; ReadyWorkloads
	// those fully rolled out. NotReady says, for each that is not, how far it is and what its
	// pods report.
	Workloads      int
	ReadyWorkloads int
	NotReady       []string
	// MissingCRDs are the CRDs the chart needs first that the cluster does not have. When any is
	// missing nothing was installed.
	MissingCRDs []string
}

// leafReleaseInstaller installs, upgrades, observes and removes one chart release on a cluster
// reached by a kubeconfig. Controllers hold one as an injected dependency; tests give a fake.
type leafReleaseInstaller interface {
	EnsureRelease(ctx context.Context, kubeconfig []byte, req helmReleaseRequest) (helmReleaseState, error)
	RemoveRelease(ctx context.Context, kubeconfig []byte, namespace, release string) error
}

// leafOperatorRequest is what the target cluster's operator should be.
type leafOperatorRequest struct {
	Namespace  string
	Version    string // the operator version: the image tag
	Chart      chartSource
	Values     map[string]any
	PullSecret *corev1.Secret // the management cluster's copy; nil copies nothing
}

// leafOperatorState is what the target cluster's operator is.
type leafOperatorState struct {
	InstalledVersion string
	ChartVersion     string
	Revision         int
	ReleaseStatus    string
	Image            string
	Replicas         int32
	ReadyReplicas    int32
	// PodProblem is why the operator's pods are not ready, in the kubelet's words. Empty when
	// they are ready or have reported nothing.
	PodProblem string
	// MissingCRDs are the prerequisites this operator version declares that the cluster lacks.
	// When any is missing the operator was not installed: it would not start.
	MissingCRDs []string
}

// leafOperatorInstaller installs, upgrades, observes and removes the operator on a cluster
// reached by a kubeconfig.
type leafOperatorInstaller interface {
	Ensure(ctx context.Context, kubeconfig []byte, req leafOperatorRequest) (leafOperatorState, error)
	Remove(ctx context.Context, kubeconfig []byte, namespace string) error
}

// The operator chart's annotation listing the CRDs that must exist on a cluster before this
// version of the operator can start (it watches their kinds, and a missing CRD is fatal by
// design: docs/operations/crd-rollout.md). Comma-separated CRD names.
const requiredCRDsAnnotation = "orama.cloud/required-crds"

func chartRequiredCRDs(chrt *chart.Chart) []string {
	var names []string
	for _, name := range strings.Split(chrt.Metadata.Annotations[requiredCRDsAnnotation], ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

var releaseVersionPattern = regexp.MustCompile(`^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`)
var notSemverIdentifier = regexp.MustCompile(`[^0-9A-Za-z-]+`)

// leafChartVersion is the chart version published for an operator version. A chart version must
// be a semantic version, and an operator version is an image tag: a release tag v0.1.0 has the
// chart 0.1.0, and a build tag such as dev-83242f9 has the chart 0.0.0-dev-83242f9. The build
// that publishes the chart (hack/push-chart.sh) uses the same rule through
// `go run ./hack/pushchart -print-version`.
func leafChartVersion(version string) string {
	if m := releaseVersionPattern.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return "0.0.0-" + strings.Trim(notSemverIdentifier.ReplaceAllString(version, "-"), "-")
}

// LeafChartVersion exposes the rule to the chart publisher.
func LeafChartVersion(version string) string { return leafChartVersion(version) }

// leafOperatorValues are the chart values for the cluster's operator: what the resource asks
// for, under what this controller sets (the image and its pull secret). Nothing else differs
// from any other cluster's operator: every cluster runs the same one, with the same kinds.
func leafOperatorValues(extra map[string]any, imageRepository, version, pullSecretName string) map[string]any {
	values := map[string]any{}
	if extra != nil {
		// A copy through JSON, so the caller's map is never written and every number is a float64,
		// the type Helm stores and returns.
		raw, _ := json.Marshal(extra)
		_ = json.Unmarshal(raw, &values)
	}
	operator, _ := values["operator"].(map[string]any)
	if operator == nil {
		operator = map[string]any{}
	}
	image, _ := operator["image"].(map[string]any)
	if image == nil {
		image = map[string]any{}
	}
	image["repository"], image["tag"] = imageRepository, version
	operator["image"] = image
	if pullSecretName != "" {
		operator["imagePullSecrets"] = []any{map[string]any{"name": pullSecretName}}
	}
	values["operator"] = operator
	return values
}

// sameValues compares chart values as Helm stores them, through JSON, so an int and the float64
// it comes back as are equal.
func sameValues(a, b map[string]any) bool {
	normalize := func(v map[string]any) any {
		if len(v) == 0 {
			return map[string]any{}
		}
		raw, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}

// registryCredentials reads the user and password for a registry host from a
// kubernetes.io/dockerconfigjson Secret. Docker Hub is known by several names.
func registryCredentials(sec *corev1.Secret, host string) (string, string, error) {
	raw := sec.Data[corev1.DockerConfigJsonKey]
	if len(raw) == 0 {
		return "", "", fmt.Errorf("Secret %s/%s has no %s key; it must be of type %s",
			sec.Namespace, sec.Name, corev1.DockerConfigJsonKey, corev1.SecretTypeDockerConfigJson)
	}
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", "", fmt.Errorf("Secret %s/%s: %s is not valid JSON: %w", sec.Namespace, sec.Name, corev1.DockerConfigJsonKey, err)
	}
	names := []string{host}
	if strings.HasSuffix(host, "docker.io") {
		names = []string{"index.docker.io", "registry-1.docker.io", "docker.io"}
	}
	keys := make([]string, 0, len(config.Auths))
	for key := range config.Auths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := config.Auths[key]
		keyHost := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
		keyHost = strings.SplitN(keyHost, "/", 2)[0]
		for _, name := range names {
			if keyHost != name {
				continue
			}
			if entry.Username != "" && entry.Password != "" {
				return entry.Username, entry.Password, nil
			}
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if user, pass, ok := strings.Cut(string(decoded), ":"); err == nil && ok {
				return user, pass, nil
			}
			return "", "", fmt.Errorf("Secret %s/%s: the entry for %s has no usable credentials", sec.Namespace, sec.Name, key)
		}
	}
	return "", "", fmt.Errorf("Secret %s/%s has no credentials for registry %s", sec.Namespace, sec.Name, host)
}

// helmLeafInstaller is the real leafOperatorInstaller: Helm's library against the leaf's API
// server, with the chart pulled from an OCI registry.
type helmLeafInstaller struct {
	mu     sync.Mutex
	charts map[string]*chart.Chart // by OCI reference; a published chart version is immutable
}

func newHelmLeafInstaller() *helmLeafInstaller {
	return &helmLeafInstaller{charts: map[string]*chart.Chart{}}
}

// kubeconfigGetter gives Helm its clients from a kubeconfig held in memory.
type kubeconfigGetter struct {
	config    clientcmd.ClientConfig
	namespace string
}

func (g *kubeconfigGetter) ToRESTConfig() (*rest.Config, error) {
	config, err := g.config.ClientConfig()
	if err != nil {
		return nil, err
	}
	config.Timeout = leafAPITimeout
	return config, nil
}

func (g *kubeconfigGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *kubeconfigGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(dc), dc, nil), nil
}

func (g *kubeconfigGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig { return g.config }

func leafClients(kubeconfig []byte, namespace string) (*kubeconfigGetter, client.Client, error) {
	raw, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("the cluster's kubeconfig cannot be read: %w", err)
	}
	// The release's namespace is the kubeconfig's default namespace: Helm puts a resource whose
	// manifest names no namespace wherever the kubeconfig says, not in the release's namespace.
	// Without this, K8ssandra 1.33.0's Deployments landed in `default` on 2026-10-12.
	config := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext,
		&clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: namespace}}, nil)
	getter := &kubeconfigGetter{config: config, namespace: namespace}
	restConfig, err := getter.ToRESTConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("the cluster's kubeconfig cannot be used: %w", err)
	}
	c, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the cluster: %w", err)
	}
	return getter, c, nil
}

func helmConfiguration(getter *kubeconfigGetter, namespace string) (*action.Configuration, error) {
	config := new(action.Configuration)
	if err := config.Init(getter, namespace, "secret", func(string, ...any) {}); err != nil {
		return nil, fmt.Errorf("helm cannot be set up against the cluster: %w", err)
	}
	return config, nil
}

// fetchChart downloads a chart, once per source for the life of the process: a pinned chart
// version does not change.
func (h *helmLeafInstaller) fetchChart(source chartSource) (*chart.Chart, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := source.String()
	if cached, ok := h.charts[key]; ok {
		return cached, nil
	}
	// Helm keeps registry credentials and repository indexes in files. They go in a directory
	// made for this download and removed after it.
	work, err := os.MkdirTemp("", "orama-operator-helm-")
	if err != nil {
		return nil, fmt.Errorf("no scratch directory for the chart download: %w", err)
	}
	defer os.RemoveAll(work)

	rc, err := registry.NewClient(registry.ClientOptWriter(io.Discard),
		registry.ClientOptCredentialsFile(filepath.Join(work, "registry.json")))
	if err != nil {
		return nil, fmt.Errorf("helm registry client: %w", err)
	}
	var loaded *chart.Chart
	if source.oci() {
		ref := strings.TrimPrefix(source.Repository, "oci://") + ":" + source.Version
		if source.User != "" {
			host := strings.SplitN(ref, "/", 2)[0]
			if err := rc.Login(host, registry.LoginOptBasicAuth(source.User, source.Pass)); err != nil {
				return nil, fmt.Errorf("registry %s refused the credentials: %w", host, err)
			}
		}
		pulled, err := rc.Pull(ref, registry.PullOptWithChart(true))
		if err != nil {
			return nil, fmt.Errorf("the chart %s cannot be pulled (is it published at this version, and may the credentials read it?): %w", ref, err)
		}
		if loaded, err = loader.LoadArchive(bytes.NewReader(pulled.Chart.Data)); err != nil {
			return nil, fmt.Errorf("the chart %s cannot be loaded: %w", ref, err)
		}
	} else {
		settings := cli.New()
		settings.RepositoryCache = filepath.Join(work, "cache")
		settings.RepositoryConfig = filepath.Join(work, "repositories.yaml")
		// An Install is only the holder of the download options here; nothing is installed by it.
		locate := action.NewInstall(new(action.Configuration))
		locate.SetRegistryClient(rc)
		locate.RepoURL, locate.Version = source.Repository, source.Version
		locate.Username, locate.Password = source.User, source.Pass
		path, err := locate.LocateChart(source.Name, settings)
		if err != nil {
			return nil, fmt.Errorf("the chart %s cannot be downloaded: %w", source, err)
		}
		if loaded, err = loader.Load(path); err != nil {
			return nil, fmt.Errorf("the chart %s cannot be loaded: %w", source, err)
		}
	}
	if loaded.Metadata.Version != source.Version {
		return nil, fmt.Errorf("the chart at %s says it is version %s", source, loaded.Metadata.Version)
	}
	h.charts[key] = loaded
	return loaded, nil
}

// missingCRDs returns which of the named CRDs the cluster does not have.
func missingCRDs(ctx context.Context, c client.Client, names []string) ([]string, error) {
	var missing []string
	for _, name := range names {
		crd := &unstructured.Unstructured{}
		crd.SetAPIVersion("apiextensions.k8s.io/v1")
		crd.SetKind("CustomResourceDefinition")
		err := c.Get(ctx, client.ObjectKey{Name: name}, crd)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, name)
		case err != nil:
			return nil, fmt.Errorf("the cluster's CRD %s cannot be read: %w", name, err)
		}
	}
	return missing, nil
}

// applyCRDManifestURL downloads a manifest and server-side applies the CRDs in it. Anything in
// the manifest that is not a CustomResourceDefinition is refused: this is for a chart's CRDs and
// nothing else.
func applyCRDManifestURL(ctx context.Context, c client.Client, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("CRD manifest %s: %w", url, err)
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return fmt.Errorf("CRD manifest %s cannot be downloaded: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("CRD manifest %s cannot be downloaded: HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("CRD manifest %s cannot be read: %w", url, err)
	}
	return applyCRDDocuments(ctx, c, url, data)
}

// applyCRDDocuments server-side applies each CustomResourceDefinition in a multi-document YAML.
func applyCRDDocuments(ctx context.Context, c client.Client, source string, data []byte) error {
	applied := 0
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
			return fmt.Errorf("%s is not valid YAML: %w", source, err)
		}
		if obj.GetKind() == "" {
			continue
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			return fmt.Errorf("%s holds a %s named %s; only CustomResourceDefinitions are applied from it", source, obj.GetKind(), obj.GetName())
		}
		if err := c.Patch(ctx, obj, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("the cluster refused CRD %s: %w", obj.GetName(), err)
		}
		applied++
	}
	if applied == 0 {
		return fmt.Errorf("%s holds no CustomResourceDefinition", source)
	}
	return nil
}

// applyCRDs server-side applies the chart's CRDs. Helm applies a chart's crds/ only on the first
// install and never on an upgrade, and a new operator version needs its new CRDs before it
// starts.
func applyCRDs(ctx context.Context, c client.Client, chrt *chart.Chart) error {
	for _, file := range chrt.CRDObjects() {
		for _, doc := range bytes.Split(file.File.Data, []byte("\n---")) {
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
				return fmt.Errorf("the chart's CRD file %s is not valid YAML: %w", file.Name, err)
			}
			if obj.GetKind() == "" {
				continue
			}
			if err := c.Patch(ctx, obj, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
				return fmt.Errorf("the cluster refused CRD %s: %w", obj.GetName(), err)
			}
		}
	}
	return nil
}

// applyNamespaceAndPullSecret creates the namespace and writes the pull secret into it.
func applyNamespaceAndPullSecret(ctx context.Context, c client.Client, namespace string, source *corev1.Secret) error {
	ns := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}
	// The namespace is created when absent and otherwise left alone: its labels and annotations
	// belong to whoever set them.
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("the cluster refused namespace %s: %w", namespace, err)
	}
	if source == nil {
		return nil
	}
	sec := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: source.Name, Namespace: namespace},
		Type:       source.Type,
		Data:       source.Data,
	}
	if err := c.Patch(ctx, sec, client.Apply, client.FieldOwner(leafFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("the cluster refused pull secret %s/%s: %w", namespace, source.Name, err)
	}
	return nil
}

// releaseVersionOf is the operator version a release runs: the image tag in its values.
func releaseVersionOf(values map[string]any) string {
	operator, _ := values["operator"].(map[string]any)
	image, _ := operator["image"].(map[string]any)
	tag, _ := image["tag"].(string)
	return tag
}

func releaseState(rel *release.Release) helmReleaseState {
	var state helmReleaseState
	if rel == nil {
		return state
	}
	state.Revision = rel.Version
	state.Values = rel.Config
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		state.ChartName, state.ChartVersion, state.AppVersion = rel.Chart.Metadata.Name, rel.Chart.Metadata.Version, rel.Chart.Metadata.AppVersion
	}
	if rel.Info != nil {
		state.Status = string(rel.Info.Status)
	}
	return state
}

// ensureRelease brings one release to the chart version and values asked for, installing or
// upgrading only when they differ from what is deployed, then reports its workloads.
func (h *helmLeafInstaller) ensureRelease(ctx context.Context, getter *kubeconfigGetter, c client.Client, req helmReleaseRequest, operatorChart bool) (helmReleaseState, error) {
	config, err := helmConfiguration(getter, req.Namespace)
	if err != nil {
		return helmReleaseState{}, err
	}
	current, err := action.NewGet(config).Run(req.Release)
	missing := errors.Is(err, driver.ErrReleaseNotFound)
	if err != nil && !missing {
		return helmReleaseState{}, fmt.Errorf("the cluster's Helm release %s cannot be read: %w", req.Release, err)
	}
	state := releaseState(current)

	upToDate := !missing && state.Status == string(release.StatusDeployed) &&
		state.ChartVersion == req.Chart.Version && sameValues(current.Config, req.Values)
	if !upToDate {
		chrt, err := h.fetchChart(req.Chart)
		if err != nil {
			return state, err
		}
		required := req.RequiredCRDs
		if operatorChart {
			required = append(append([]string{}, required...), chartRequiredCRDs(chrt)...)
		}
		if state.MissingCRDs, err = missingCRDs(ctx, c, required); err != nil {
			return state, err
		}
		if len(state.MissingCRDs) > 0 {
			return state, nil
		}
		for _, url := range req.CRDURLs {
			if err := applyCRDManifestURL(ctx, c, url); err != nil {
				return state, err
			}
		}
		// Helm applies a chart's crds/ only on the first install and never on an upgrade, and a
		// new version needs its new CRDs before it starts. They are applied here every time.
		if err := applyCRDs(ctx, c, chrt); err != nil {
			return state, err
		}
		if missing {
			install := action.NewInstall(config)
			install.ReleaseName, install.Namespace = req.Release, req.Namespace
			install.CreateNamespace = true
			install.SkipCRDs = true
			install.Timeout = leafAPITimeout
			installed, err := install.RunWithContext(ctx, chrt, req.Values)
			if err != nil {
				return state, fmt.Errorf("helm install of %s on the cluster failed: %w", req.Release, err)
			}
			current = installed
		} else {
			upgrade := action.NewUpgrade(config)
			upgrade.Namespace = req.Namespace
			upgrade.SkipCRDs = true
			upgrade.MaxHistory = 10
			upgrade.Timeout = leafAPITimeout
			upgraded, err := upgrade.RunWithContext(ctx, req.Release, chrt, req.Values)
			if err != nil {
				return state, fmt.Errorf("helm upgrade of %s on the cluster failed: %w", req.Release, err)
			}
			current = upgraded
		}
		state = releaseState(current)
	}
	return observeWorkloads(ctx, c, current, state)
}

func (h *helmLeafInstaller) EnsureRelease(ctx context.Context, kubeconfig []byte, req helmReleaseRequest) (helmReleaseState, error) {
	getter, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return helmReleaseState{}, err
	}
	return h.ensureRelease(ctx, getter, c, req, false)
}

func (h *helmLeafInstaller) RemoveRelease(ctx context.Context, kubeconfig []byte, namespace, name string) error {
	getter, _, err := leafClients(kubeconfig, namespace)
	if err != nil {
		return err
	}
	config, err := helmConfiguration(getter, namespace)
	if err != nil {
		return err
	}
	uninstall := action.NewUninstall(config)
	uninstall.Timeout = leafAPITimeout
	if _, err := uninstall.Run(name); err != nil && !errors.Is(err, driver.ErrReleaseNotFound) && !apierrors.IsNotFound(err) {
		return fmt.Errorf("helm uninstall of %s on the cluster failed: %w", name, err)
	}
	return nil
}

// observeWorkloads reads each Deployment, StatefulSet and DaemonSet the release's manifest
// declares and, for one that is not fully rolled out, what its pods report.
func observeWorkloads(ctx context.Context, c client.Client, rel *release.Release, state helmReleaseState) (helmReleaseState, error) {
	if rel == nil {
		return state, nil
	}
	for _, doc := range strings.Split(rel.Manifest, "\n---") {
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil || head.APIVersion != "apps/v1" {
			continue
		}
		namespace := head.Metadata.Namespace
		if namespace == "" {
			namespace = rel.Namespace
		}
		key := client.ObjectKey{Namespace: namespace, Name: head.Metadata.Name}
		var want, ready int32
		var selector *metav1.LabelSelector
		var err error
		switch head.Kind {
		case "Deployment":
			var w appsv1.Deployment
			if err = c.Get(ctx, key, &w); err == nil {
				want = 1
				if w.Spec.Replicas != nil {
					want = *w.Spec.Replicas
				}
				if w.Status.ObservedGeneration >= w.Generation {
					ready = min(w.Status.ReadyReplicas, w.Status.UpdatedReplicas)
				}
				selector = w.Spec.Selector
			}
		case "StatefulSet":
			var w appsv1.StatefulSet
			if err = c.Get(ctx, key, &w); err == nil {
				want = 1
				if w.Spec.Replicas != nil {
					want = *w.Spec.Replicas
				}
				if w.Status.ObservedGeneration >= w.Generation {
					ready = min(w.Status.ReadyReplicas, w.Status.UpdatedReplicas)
				}
				selector = w.Spec.Selector
			}
		case "DaemonSet":
			var w appsv1.DaemonSet
			if err = c.Get(ctx, key, &w); err == nil {
				want = w.Status.DesiredNumberScheduled
				if w.Status.ObservedGeneration >= w.Generation {
					ready = min(w.Status.NumberReady, w.Status.UpdatedNumberScheduled)
				}
				selector = w.Spec.Selector
			}
		default:
			continue
		}
		state.Workloads++
		name := head.Kind + " " + namespace + "/" + head.Metadata.Name
		switch {
		case apierrors.IsNotFound(err):
			state.NotReady = append(state.NotReady, name+" does not exist on the cluster")
			continue
		case err != nil:
			return state, fmt.Errorf("%s cannot be read on the cluster: %w", name, err)
		case ready >= want:
			state.ReadyWorkloads++
			continue
		}
		line := fmt.Sprintf("%s has %d of %d ready", name, ready, want)
		if selector != nil && len(selector.MatchLabels) > 0 {
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(selector.MatchLabels)); err != nil {
				return state, fmt.Errorf("the pods of %s cannot be read on the cluster: %w", name, err)
			}
			if problem := podProblem(pods.Items); problem != "" {
				line += " (" + problem + ")"
			}
		}
		state.NotReady = append(state.NotReady, line)
	}
	return state, nil
}

// Ensure installs the operator: the namespace and pull secret first, then the release, which is
// not installed while a CRD the operator's chart declares as required is missing.
func (h *helmLeafInstaller) Ensure(ctx context.Context, kubeconfig []byte, req leafOperatorRequest) (leafOperatorState, error) {
	getter, c, err := leafClients(kubeconfig, req.Namespace)
	if err != nil {
		return leafOperatorState{}, err
	}
	// Written on every pass, so a rotated pull secret reaches the leaf.
	if err := applyNamespaceAndPullSecret(ctx, c, req.Namespace, req.PullSecret); err != nil {
		return leafOperatorState{}, err
	}
	rel, err := h.ensureRelease(ctx, getter, c, helmReleaseRequest{
		Release: leafOperatorRelease, Namespace: req.Namespace, Chart: req.Chart, Values: req.Values,
	}, true)
	state := leafOperatorState{
		InstalledVersion: releaseVersionOf(rel.Values), ChartVersion: rel.ChartVersion,
		Revision: rel.Revision, ReleaseStatus: rel.Status, MissingCRDs: rel.MissingCRDs,
	}
	if err != nil || len(rel.MissingCRDs) > 0 {
		return state, err
	}
	return observeLeafOperator(ctx, c, req.Namespace, state)
}

// observeLeafOperator reads the operator's Deployment on the leaf and, when its pods are not
// ready, what the kubelet says is wrong with them.
func observeLeafOperator(ctx context.Context, c client.Client, namespace string, state leafOperatorState) (leafOperatorState, error) {
	selector := client.MatchingLabels{leafOperatorNameLabel: leafOperatorNameValue}
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(namespace), selector); err != nil {
		return state, fmt.Errorf("the cluster's operator Deployment cannot be read: %w", err)
	}
	if len(deployments.Items) == 0 {
		return state, nil
	}
	deployment := deployments.Items[0]
	if containers := deployment.Spec.Template.Spec.Containers; len(containers) > 0 {
		state.Image = containers[0].Image
	}
	state.Replicas = 1
	if deployment.Spec.Replicas != nil {
		state.Replicas = *deployment.Spec.Replicas
	}
	// Ready means ready on the pod template now asked for, not on the one being replaced.
	if deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.UpdatedReplicas >= state.Replicas {
		state.ReadyReplicas = min(deployment.Status.ReadyReplicas, deployment.Status.UpdatedReplicas)
	}
	if state.ReadyReplicas >= state.Replicas {
		return state, nil
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace), selector); err != nil {
		return state, fmt.Errorf("the cluster's operator pods cannot be read: %w", err)
	}
	state.PodProblem = podProblem(pods.Items)
	return state, nil
}

// podProblem says why pods are not ready: a container that cannot start, one that keeps exiting,
// or a pod the scheduler cannot place.
func podProblem(pods []corev1.Pod) string {
	seen := map[string]bool{}
	var problems []string
	add := func(text string) {
		text = strings.Join(strings.Fields(text), " ")
		if len(text) > 500 {
			text = text[:500] + "..."
		}
		if text != "" && !seen[text] {
			seen[text] = true
			problems = append(problems, text)
		}
	}
	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
				add("pod " + pod.Name + " " + condition.Reason + ": " + condition.Message)
			}
		}
		for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			switch {
			case status.State.Waiting != nil && status.State.Waiting.Reason != "ContainerCreating" && status.State.Waiting.Reason != "PodInitializing":
				text := "container " + status.Name + " " + status.State.Waiting.Reason + ": " + status.State.Waiting.Message
				if terminated := status.LastTerminationState.Terminated; terminated != nil {
					text += fmt.Sprintf(" (last exit %d %s %s)", terminated.ExitCode, terminated.Reason, terminated.Message)
				}
				add(text)
			case status.State.Terminated != nil && status.State.Terminated.ExitCode != 0:
				add(fmt.Sprintf("container %s exited %d %s: %s", status.Name, status.State.Terminated.ExitCode,
					status.State.Terminated.Reason, status.State.Terminated.Message))
			}
		}
	}
	return strings.Join(problems, "; ")
}

// Remove uninstalls the operator's release. The CRDs, and so every resource of the operator's
// kinds, stay: removing an operator must not delete what it was running. A release that is
// already gone is not an error.
func (h *helmLeafInstaller) Remove(ctx context.Context, kubeconfig []byte, namespace string) error {
	return h.RemoveRelease(ctx, kubeconfig, namespace, leafOperatorRelease)
}
