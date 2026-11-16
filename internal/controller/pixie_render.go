// SPDX-License-Identifier: MIT

package controller

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The operator installs Pixie itself (operator#15, #456): no Pixie operator, no OLM, no Vizier
// updater. It embeds the kustomize output of one pinned commit of oramasearch/pixie
// (hack/fetch-pixie-manifests.sh), decodes it into typed objects, refuses a bundle that is not
// exactly the expected object set, and sets every value on the decoded objects, never as text:
//
//   - each image becomes the release's reference by digest (#455), or rendering fails;
//   - every pod pulls with the cluster's own pull Secret, has requests and limits, and every Go
//     binary gets GOMEMLIMIT from the same number as its memory limit;
//   - what calls out of the cluster (Sentry, Segment, LaunchDarkly, BigQuery, GCS, the public
//     artifact manifest and script bundle) is turned off or pointed inside it, so the install runs
//     air-gapped;
//   - Vizier's auto-update is off and its updater, and the cert provisioner Job, are never applied:
//     the operator mints the certificates and the version only changes with the release.

//go:embed assets/pixie/vizier.yaml assets/pixie/vizier-nats.yaml assets/pixie/cloud.yaml assets/pixie/cloud-nats.yaml assets/pixie/COMMIT
var pixieAssets embed.FS

// pixieManifestCommit is the oramasearch/pixie commit the embedded manifests are rendered from
// (assets/pixie/COMMIT; TestPixieManifestCommitMatchesAssets). A release built from another commit
// is refused.
const pixieManifestCommit = "4b445e0b7907c79d034bcb4a227bbaede94d370a"

const (
	pixieManagedLabel  = "orama.cloud/pixie"
	pixieManagedVizier = "vizier"
	pixieManagedCloud  = "cloud"
	// pixieManagedMetrics marks the metrics objects (the hub Prometheus, a leaf's federation proxy),
	// applied apart from Pixie so its prune never touches them.
	pixieManagedMetrics = "metrics"
	pixieFieldOwner     = "orama-operator-pixie"
	pixieComponentLabel = "orama.cloud/pixie-component"
)

// pixieImageKeys is every image a release must name, and nothing else. Upstream's image names on
// the left of pixieUpstreamImages map to these.
var pixieImageKeys = []string{
	"cloud-api",
	"cloud-artifact-tracker",
	"cloud-auth",
	"cloud-config-manager",
	"cloud-cron-script",
	"cloud-indexer",
	"cloud-plugin",
	"cloud-profile",
	"cloud-project-manager",
	"cloud-proxy",
	"cloud-scriptmgr",
	"cloud-vzconn",
	"cloud-vzmgr",
	"curl",
	"envoy",
	"hydra",
	"hydra-cli",
	"kratos",
	"nats",
	"opensearch",
	"postgres",
	"script-bundle",
	"vizier-cloud-connector",
	"vizier-kelvin",
	"vizier-metadata",
	"vizier-pem",
	"vizier-query-broker",
}

// pixieUpstreamImages maps each image repository the vendored manifests name (the reference before
// its tag or digest) to the release key that replaces it. TestPixieBundlesUseOnlyMappedImages fails
// when a re-vendored bundle names another.
var pixieUpstreamImages = map[string]string{
	"vizier-pem_image":                                   "vizier-pem",
	"vizier-kelvin_image":                                "vizier-kelvin",
	"vizier-metadata_server_image":                       "vizier-metadata",
	"vizier-query_broker_server_image":                   "vizier-query-broker",
	"vizier-cloud_connector_server_image":                "vizier-cloud-connector",
	"ghcr.io/pixie-io/pixie-oss-pixie-dev-public-curl":   "curl",
	"ghcr.io/pixie-io/nats":                              "nats",
	"envoyproxy/envoy":                                   "envoy",
	"oryd/hydra#sqlite":                                  "hydra",
	"oryd/hydra#alpine":                                  "hydra-cli",
	"oryd/kratos":                                        "kratos",
	"gcr.io/pixie-oss/pixie-prod/cloud-api_server_image": "cloud-api",
	"gcr.io/pixie-oss/pixie-prod/cloud-artifact_tracker_server_image": "cloud-artifact-tracker",
	"gcr.io/pixie-oss/pixie-prod/cloud-auth_server_image":             "cloud-auth",
	"gcr.io/pixie-oss/pixie-prod/cloud-config_manager_server_image":   "cloud-config-manager",
	"gcr.io/pixie-oss/pixie-prod/cloud-cron_script_server_image":      "cloud-cron-script",
	"gcr.io/pixie-oss/pixie-prod/cloud-indexer_server_image":          "cloud-indexer",
	"gcr.io/pixie-oss/pixie-prod/cloud-plugin_server_image":           "cloud-plugin",
	"gcr.io/pixie-oss/pixie-prod/cloud-profile_server_image":          "cloud-profile",
	"gcr.io/pixie-oss/pixie-prod/cloud-project_manager_server_image":  "cloud-project-manager",
	"gcr.io/pixie-oss/pixie-prod/cloud-proxy_server_image":            "cloud-proxy",
	"gcr.io/pixie-oss/pixie-prod/cloud-scriptmgr_server_image":        "cloud-scriptmgr",
	"gcr.io/pixie-oss/pixie-prod/cloud-vzconn_server_image":           "cloud-vzconn",
	"gcr.io/pixie-oss/pixie-prod/cloud-vzmgr_server_image":            "cloud-vzmgr",
}

// upstreamImageRepository is an image reference without its tag or digest. Hydra's two variants
// share a repository and differ by tag, so they are told apart by it.
func upstreamImageRepository(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	repository := ref
	tag := ""
	if slash := strings.LastIndex(ref, "/"); strings.LastIndex(ref, ":") > slash {
		colon := strings.LastIndex(ref, ":")
		repository, tag = ref[:colon], ref[colon+1:]
	}
	if repository == "oryd/hydra" {
		if strings.HasSuffix(tag, "-sqlite") {
			return repository + "#sqlite"
		}
		return repository + "#alpine"
	}
	return repository
}

// pixieReleaseProblems says what a release lacks to be installed, or nothing.
func pixieReleaseProblems(release *oramav1alpha1.PixieRelease) []string {
	if release == nil {
		return []string{"the provider has no pixie release"}
	}
	var problems []string
	if release.Commit != pixieManifestCommit {
		problems = append(problems, fmt.Sprintf("the release is built from oramasearch/pixie %s, but this operator vendors the manifests of %s: build the images from that commit (#455) or move the operator", release.Commit, pixieManifestCommit))
	}
	want := map[string]bool{}
	for _, key := range pixieImageKeys {
		want[key] = true
	}
	var missing, unknown []string
	for _, key := range pixieImageKeys {
		if release.Images[key] == "" {
			missing = append(missing, key)
		}
	}
	for key, ref := range release.Images {
		if !want[key] {
			unknown = append(unknown, key)
			continue
		}
		if !imageReferenceByDigest(string(ref)) {
			problems = append(problems, fmt.Sprintf("image %s is %q, not repository@sha256:<digest>", key, ref))
		} else if name, _, _ := strings.Cut(string(ref), "@"); key == "postgres" && strings.LastIndex(name, ":") < strings.LastIndex(name, "/") {
			// CloudNativePG reads the PostgreSQL version from the tag and refuses a digest alone.
			problems = append(problems, fmt.Sprintf("image postgres is %q, not repository:<version>@sha256:<digest>", ref))
		}
	}
	sort.Strings(unknown)
	if len(missing) > 0 {
		problems = append(problems, "the release names no image for "+strings.Join(missing, ", "))
	}
	if len(unknown) > 0 {
		problems = append(problems, "the release names images this operator does not render: "+strings.Join(unknown, ", "))
	}
	return problems
}

func imageReferenceByDigest(ref string) bool {
	repository, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok || repository == "" || len(digest) != 64 || strings.ContainsAny(repository, " \t@") {
		return false
	}
	for _, r := range digest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

var pixieDecoder = func() runtime.Decoder {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return serializer.NewCodecFactory(scheme).UniversalDeserializer()
}()

// decodePixieAsset decodes one embedded bundle into typed objects, in file order.
func decodePixieAsset(name string) ([]client.Object, error) {
	data, err := pixieAssets.ReadFile("assets/pixie/" + name)
	if err != nil {
		return nil, err
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var objects []client.Object
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(bytes.TrimSpace(stripYAMLComments(doc))) == 0 {
			continue
		}
		decoded, _, err := pixieDecoder.Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		object, ok := decoded.(client.Object)
		if !ok {
			return nil, fmt.Errorf("%s: %T is not an object", name, decoded)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func stripYAMLComments(doc []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.Split(doc, []byte("\n")) {
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && trimmed[0] != '#' {
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// pixieObjectKey identifies one vendored object.
type pixieObjectKey struct{ Kind, Name string }

func keyOf(object client.Object) pixieObjectKey {
	return pixieObjectKey{Kind: kindOf(object), Name: object.GetName()}
}

func kindOf(object client.Object) string {
	switch object.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *batchv1.Job:
		return "Job"
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *corev1.Secret:
		return "Secret"
	case *corev1.Service:
		return "Service"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *corev1.PersistentVolumeClaim:
		return "PersistentVolumeClaim"
	case *rbacv1.Role:
		return "Role"
	case *rbacv1.RoleBinding:
		return "RoleBinding"
	case *rbacv1.ClusterRole:
		return "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	case *policyv1.PodDisruptionBudget:
		return "PodDisruptionBudget"
	}
	return fmt.Sprintf("%T", object)
}

// pixieBundle decodes the named assets and keeps exactly the objects in keep, failing when the
// vendored files hold anything that is neither kept nor known to be dropped. Re-vendoring a commit
// that adds or renames an object fails here and in the tests until the lists are updated.
func pixieBundle(assets []string, keep, drop []pixieObjectKey) ([]client.Object, error) {
	keepSet := map[pixieObjectKey]bool{}
	for _, key := range keep {
		keepSet[key] = true
	}
	dropSet := map[pixieObjectKey]bool{}
	for _, key := range drop {
		dropSet[key] = true
	}
	var kept []client.Object
	seen := map[pixieObjectKey]bool{}
	for _, asset := range assets {
		objects, err := decodePixieAsset(asset)
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			key := keyOf(object)
			switch {
			case keepSet[key]:
				kept = append(kept, object)
				seen[key] = true
			case dropSet[key]:
			default:
				return nil, fmt.Errorf("the vendored Pixie manifests hold %s %s, which is neither rendered nor known to be dropped", key.Kind, key.Name)
			}
		}
	}
	for _, key := range keep {
		if !seen[key] {
			return nil, fmt.Errorf("the vendored Pixie manifests have no %s %s", key.Kind, key.Name)
		}
	}
	return kept, nil
}

// podSpecOf is the pod template of a workload, or nil.
func podSpecOf(object client.Object) (*corev1.PodSpec, *map[string]string) {
	switch o := object.(type) {
	case *appsv1.Deployment:
		return &o.Spec.Template.Spec, &o.Spec.Template.Labels
	case *appsv1.StatefulSet:
		return &o.Spec.Template.Spec, &o.Spec.Template.Labels
	case *appsv1.DaemonSet:
		return &o.Spec.Template.Spec, &o.Spec.Template.Labels
	case *batchv1.Job:
		return &o.Spec.Template.Spec, &o.Spec.Template.Labels
	}
	return nil, nil
}

// setReleaseImages replaces every container's image by the release's reference for it.
func setReleaseImages(object client.Object, images map[string]oramav1alpha1.ImageDigestReference) error {
	spec, _ := podSpecOf(object)
	if spec == nil {
		return nil
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			key, ok := pixieUpstreamImages[upstreamImageRepository(containers[i].Image)]
			if !ok {
				return fmt.Errorf("%s %s container %s runs %q, which no release image replaces", kindOf(object), object.GetName(), containers[i].Name, containers[i].Image)
			}
			ref := images[key]
			if ref == "" {
				return fmt.Errorf("the release names no image %s", key)
			}
			containers[i].Image = string(ref)
			containers[i].ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	return nil
}

// pixieBudget is one container's memory and CPU. The memory limit is also the binary's own cap:
// GOMEMLIMIT for a Go service, the JVM heap for OpenSearch, the table store for the node agent.
type pixieBudget struct {
	CPURequest, CPULimit, MemoryRequest, MemoryLimit string
	// GoRuntime marks a Go binary, which gets GOMEMLIMIT at 90% of the memory limit.
	GoRuntime bool
}

func (b pixieBudget) requirements() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(b.CPURequest),
			corev1.ResourceMemory: resource.MustParse(b.MemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(b.CPULimit),
			corev1.ResourceMemory: resource.MustParse(b.MemoryLimit),
		},
	}
}

// goMemLimit is 90% of a memory limit, in bytes: the Go runtime collects harder before the cgroup
// kills it.
func goMemLimit(limit resource.Quantity) string {
	return strconv.FormatInt(limit.Value()/10*9, 10)
}

// applyBudgets sets each container's resources from budgets, keyed by "<workload>/<container>".
// A container with no budget is an error, so a re-vendored bundle cannot add an uncapped one.
func applyBudgets(object client.Object, budgets map[string]pixieBudget) error {
	spec, _ := podSpecOf(object)
	if spec == nil {
		return nil
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			key := object.GetName() + "/" + containers[i].Name
			budget, ok := budgets[key]
			if !ok {
				return fmt.Errorf("container %s has no resource budget", key)
			}
			if budget.MemoryLimit == "" {
				// Set by the caller from the spec (the node agent).
				continue
			}
			containers[i].Resources = budget.requirements()
			if budget.GoRuntime {
				setEnv(&containers[i], "GOMEMLIMIT", goMemLimit(resource.MustParse(budget.MemoryLimit)))
			}
		}
	}
	return nil
}

// setEnv sets a literal environment variable, replacing any earlier definition of the name.
func setEnv(c *corev1.Container, name, value string) {
	setEnvVar(c, corev1.EnvVar{Name: name, Value: value})
}

func setEnvVar(c *corev1.Container, value corev1.EnvVar) {
	for i := range c.Env {
		if c.Env[i].Name == value.Name {
			c.Env[i] = value
			return
		}
	}
	c.Env = append(c.Env, value)
}

func removeEnv(c *corev1.Container, name string) {
	kept := c.Env[:0]
	for _, env := range c.Env {
		if env.Name != name {
			kept = append(kept, env)
		}
	}
	c.Env = kept
}

// containerNamed returns the named container of a pod spec, init containers included.
func containerNamed(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == name {
			return &spec.InitContainers[i]
		}
	}
	return nil
}

func removeContainer(spec *corev1.PodSpec, name string) {
	kept := spec.Containers[:0]
	for _, c := range spec.Containers {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	spec.Containers = kept
}

// stampPixieObject labels an object as the operator's, sets its namespace when it has one, and
// gives every pod the pull Secret and component label.
func stampPixieObject(object client.Object, managed, namespace, pullSecret string) {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[pixieManagedLabel] = managed
	labels["app.kubernetes.io/managed-by"] = "orama-operator"
	object.SetLabels(labels)
	switch object.(type) {
	case *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
		object.SetNamespace("")
	default:
		object.SetNamespace(namespace)
	}
	if sa, ok := object.(*corev1.ServiceAccount); ok {
		sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	}
	spec, podLabels := podSpecOf(object)
	if spec == nil {
		return
	}
	spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	if *podLabels == nil {
		*podLabels = map[string]string{}
	}
	(*podLabels)[pixieManagedLabel] = managed
	(*podLabels)[pixieComponentLabel] = object.GetName()
}

// bindingsToNamespace points every ServiceAccount subject of a binding at namespace.
func bindingsToNamespace(object client.Object, namespace string) {
	var subjects []rbacv1.Subject
	switch o := object.(type) {
	case *rbacv1.RoleBinding:
		subjects = o.Subjects
	case *rbacv1.ClusterRoleBinding:
		subjects = o.Subjects
	}
	for i := range subjects {
		if subjects[i].Kind == rbacv1.ServiceAccountKind {
			subjects[i].Namespace = namespace
		}
	}
}

// stampPixieConfigHash gives every pod template a fingerprint of the rendered ConfigMaps it reads
// (envFrom, env valueFrom, volumes), so a configuration change rolls exactly the pods that read it.
// Pixie's services read their configuration once, at start: a ConfigMap changed under a running pod
// changes nothing (v0.11.10's script bundle URL reached the API server, whose Deployment changed,
// but not the proxy, whose did not).
func stampPixieConfigHash(objects []client.Object) {
	configs := map[string]map[string]string{}
	for _, object := range objects {
		if cm, ok := object.(*corev1.ConfigMap); ok {
			configs[cm.Name] = cm.Data
		}
	}
	for _, object := range objects {
		spec, _ := podSpecOf(object)
		if spec == nil {
			continue
		}
		read := map[string]bool{}
		for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
			for _, c := range containers {
				for _, from := range c.EnvFrom {
					if from.ConfigMapRef != nil {
						read[from.ConfigMapRef.Name] = true
					}
				}
				for _, env := range c.Env {
					if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
						read[env.ValueFrom.ConfigMapKeyRef.Name] = true
					}
				}
			}
		}
		for _, volume := range spec.Volumes {
			if volume.ConfigMap != nil {
				read[volume.ConfigMap.Name] = true
			}
			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.ConfigMap != nil {
						read[source.ConfigMap.Name] = true
					}
				}
			}
		}
		names := make([]string, 0, len(read))
		for name := range read {
			if _, rendered := configs[name]; rendered {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		hash := sha256.New()
		for _, name := range names {
			keys := make([]string, 0, len(configs[name]))
			for key := range configs[name] {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			fmt.Fprintf(hash, "%s\x00", name)
			for _, key := range keys {
				fmt.Fprintf(hash, "%s=%s\x00", key, configs[name][key])
			}
		}
		podTemplateAnnotations(object)[pixieConfigAnnotation] = hex.EncodeToString(hash.Sum(nil))[:16]
	}
}
