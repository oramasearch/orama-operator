/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Self-hosted Pixie Cloud, rendered for one PixieCloud (operator#15, #456). The vendored services
// and Ory are kept; their dependencies are not taken from upstream:
//
//   - Elasticsearch through Elastic's ECK operator, and Kibana (Elastic License, SSPL or AGPL by
//     version), are replaced by OpenSearch (Apache-2.0), run by the operator as a StatefulSet
//     (pixie_cloud_search.go). The indexer runs with manual index management, so it never calls
//     Elastic's ILM API, and the operator creates its index.
//   - The single Postgres Deployment shared by every service is replaced by a CloudNativePG cluster
//     with one database and one login role per service (pixie_cloud_db.go).
//   - The metrics server is not run: its only job is exporting usage to Google BigQuery.

const (
	pixieDomainConfig      = "pl-domain-config"
	pixieServiceConfig     = "pl-service-config"
	pixieProxyNginxConfig  = "pl-proxy-nginx-config"
	pixieServerCertsSecret = "service-tls-server-certs"
	pixieClientCertsSecret = "service-tls-client-certs"
	pixieProxyCertsSecret  = "cloud-proxy-tls-certs"
	pixieAuthSecret        = "cloud-auth-secrets"
	pixieSessionSecret     = "cloud-session-secrets"
	pixieHydraSecret       = "pl-hydra-secrets"
	pixieOIDCSecret        = "cloud-oidc-secrets"
	pixieSearchSecret      = "pixie-search"
	pixieSearchService     = "pixie-search"
	pixieSearchPort        = 9200
	pixieScriptsService    = "pixie-scripts"
	pixieScriptsPort       = 8080
	pixieScriptsBucket     = "pixie-scripts"
	pixieMetadataIndex     = "md_entities_9"
	pixieVzconnService     = "vzconn-service"
	pixieVzconnPort        = 51600
	pixieCloudCASecret     = "pixie-cloud-ca"
	pixieServiceCASecret   = "pixie-service-ca"
	pixieAdminSecret       = "pixie-admin"
	pixieUserPrefix        = "pixie-user-"
	pixieDeployKeyPrefix   = "pixie-deploy-key-"
)

// pixieCloudObjects is exactly the vendored Cloud the operator applies.
var pixieCloudObjects = []pixieObjectKey{
	{"Role", "pl-kuberesolver-role"},
	{"RoleBinding", "pl-kuberesolver-role-binding"},
	{"ConfigMap", "hydra-config"},
	{"ConfigMap", "kratos-config"},
	{"ConfigMap", "pl-analytics-config"},
	{"ConfigMap", "pl-artifact-config"},
	{"ConfigMap", "pl-contact-config"},
	{"ConfigMap", "pl-db-config"},
	{"ConfigMap", pixieDomainConfig},
	{"ConfigMap", "pl-indexer-config"},
	{"ConfigMap", "pl-oauth-config"},
	{"ConfigMap", "pl-ory-service-config"},
	{"ConfigMap", pixieProxyNginxConfig},
	{"ConfigMap", "pl-script-bundles-config"},
	{"ConfigMap", "pl-scriptmgr-config"},
	{"ConfigMap", pixieServiceConfig},
	{"ConfigMap", "pl-support-access-config"},
	{"ConfigMap", "pl-tls-config"},
	{"ConfigMap", "proxy-envoy-config"},
	{"Service", "api-service"},
	{"Service", "artifact-tracker-service"},
	{"Service", "auth-service"},
	{"Service", "cloud-proxy-service"},
	{"Service", "config-manager-service"},
	{"Service", "cron-script-service"},
	{"Service", "hydra"},
	{"Service", "kratos"},
	{"Service", "plugin-service"},
	{"Service", "profile-service"},
	{"Service", "project-manager-service"},
	{"Service", "scriptmgr-service"},
	{"Service", pixieVzconnService},
	{"Service", "vzmgr-service"},
	{"Deployment", "api-server"},
	{"Deployment", "artifact-tracker-server"},
	{"Deployment", "auth-server"},
	{"Deployment", "cloud-proxy"},
	{"Deployment", "config-manager-server"},
	{"Deployment", "cron-script-server"},
	{"Deployment", "hydra"},
	{"Deployment", "indexer-server"},
	{"Deployment", "kratos"},
	{"Deployment", "plugin-server"},
	{"Deployment", "profile-server"},
	{"Deployment", "project-manager-server"},
	{"Deployment", "scriptmgr-server"},
	{"Deployment", "vzconn-server"},
	{"Deployment", "vzmgr-server"},
	{"ServiceAccount", "nats"},
	{"ServiceAccount", "nats-server"},
	{"ClusterRole", "plc:nats-server"},
	{"ClusterRoleBinding", "plc:nats-server-binding"},
	{"ConfigMap", "nats-config"},
	{"ConfigMap", "pl-jetstream-config"},
	{"Service", "pl-nats"},
	{"StatefulSet", "pl-nats"},
	{"PodDisruptionBudget", "pl-nats-pdb"},
}

// pixieCloudDropped is what the vendored Cloud holds and the operator never applies.
var pixieCloudDropped = []pixieObjectKey{
	// Exports usage to Google BigQuery.
	{"Deployment", "metrics-server"},
	{"ConfigMap", "pl-bq-config"},
	// Downloads plugin definitions from GitHub (pixie-io/pixie-plugin) at run time, which an
	// air-gapped hub cannot do. Export plugins wait for a mirrored plugin artifact (#455).
	{"Job", "plugin-db-updater-job"},
}

// pixieDatabaseServices are the services that keep data in Postgres, each with its own database
// and login role. vzconn and the artifact tracker have database settings upstream but never open a
// connection, so they get no credentials at all.
var pixieDatabaseServices = map[string]string{
	"auth-server":            "auth",
	"cron-script-server":     "cronscript",
	"hydra":                  "hydra",
	"kratos":                 "kratos",
	"plugin-server":          "plugin",
	"profile-server":         "profile",
	"project-manager-server": "projectmanager",
	"vzmgr-server":           "vzmgr",
}

// pixieGoBudget is the budget of one small Go service.
var pixieGoBudget = pixieBudget{CPURequest: "50m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi", GoRuntime: true}

// pixieCloudBudgets caps every Cloud container.
var pixieCloudBudgets = map[string]pixieBudget{
	"api-server/api-server":                           {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
	"artifact-tracker-server/artifact-tracker-server": pixieGoBudget,
	"auth-server/auth-server":                         pixieGoBudget,
	"cloud-proxy/cloud-proxy-server":                  {CPURequest: "50m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi"},
	"cloud-proxy/envoy":                               {CPURequest: "50m", CPULimit: "500m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"},
	"config-manager-server/config-manager-server":     pixieGoBudget,
	"cron-script-server/cron-script-server":           pixieGoBudget,
	"hydra/migrate":                                   pixieGoBudget,
	"hydra/server":                                    pixieGoBudget,
	"hydra/client-create-or-update":                   {CPURequest: "10m", CPULimit: "100m", MemoryRequest: "32Mi", MemoryLimit: "64Mi"},
	"indexer-server/indexer-server":                   {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
	"kratos/migrate":                                  pixieGoBudget,
	"kratos/server":                                   pixieGoBudget,
	"plugin-server/plugin-server":                     pixieGoBudget,
	"profile-server/profile-server":                   pixieGoBudget,
	"project-manager-server/project-manager-server":   pixieGoBudget,
	"scriptmgr-server/scriptmgr-server":               pixieGoBudget,
	"vzconn-server/vzconn-server":                     {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
	"vzmgr-server/vzmgr-server":                       {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
	"pl-nats/pl-nats":                                 {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
}

// pixieCloudRender is everything rendering needs beyond the spec.
type pixieCloudRender struct {
	Cloud *oramav1alpha1.PixieCloud
	// CertsHash changes whenever a certificate changes, and rolls every pod that reads them.
	CertsHash string
	// DatabaseHost is the CloudNativePG read-write Service.
	DatabaseHost string
}

// renderPixieCloud is the vendored Cloud for one PixieCloud, transformed. The operator's own parts
// (OpenSearch, the script bundle server, the database, the configuration Pixie expects from
// cloud_deps) are rendered beside it by pixieCloudSupportObjects.
func renderPixieCloud(input pixieCloudRender) ([]client.Object, error) {
	cloud := input.Cloud
	if problems := pixieReleaseProblems(&cloud.Spec.Release); len(problems) > 0 {
		return nil, fmt.Errorf("%s", problems[0])
	}
	objects, err := pixieBundle([]string{"cloud.yaml", "cloud-nats.yaml"}, pixieCloudObjects, pixieCloudDropped)
	if err != nil {
		return nil, err
	}
	objects = append(objects, pixieCloudDepsConfig()...)
	for _, object := range objects {
		stampPixieObject(object, pixieManagedCloud, oramav1alpha1.PixieCloudNamespace, cloud.Spec.PullSecretName)
		bindingsToNamespace(object, oramav1alpha1.PixieCloudNamespace)
		if err := setReleaseImages(object, cloud.Spec.Release.Images); err != nil {
			return nil, err
		}
		if spec, _ := podSpecOf(object); spec != nil && object.GetName() == "kratos" {
			// The upstream container creates admin@default.com with password "admin". The operator
			// creates the administrator itself, with a generated password (pixie_cloud_bootstrap.go).
			removeContainer(spec, "admin-create-if-not-exists")
		}
		if err := applyBudgets(object, pixieCloudBudgets); err != nil {
			return nil, err
		}
		if err := transformPixieCloudObject(object, input); err != nil {
			return nil, err
		}
	}
	stampPixieConfigHash(objects)
	return objects, nil
}

// pixieCloudDepsConfig is the configuration upstream ships in cloud_deps/public/configs.yaml that
// the services read, with every outside service off.
func pixieCloudDepsConfig() []client.Object {
	config := func(name string, data map[string]string) client.Object {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}
	}
	return []client.Object{
		config("pl-announcement-config", map[string]string{"ANNOUNCEMENT_ENABLED": "false", "ANNOUNCE_WIDGET_URL": ""}),
		config("pl-auth-connector-config", map[string]string{"PL_AUTH_CONNECTOR_NAME": "", "PL_AUTH_CONNECTOR_CALLBACK_URL": ""}),
		config("pl-errors-config", map[string]string{"PL_SENTRY_DSN": ""}),
		config("pl-ld-config", map[string]string{"PL_LD_CLIENT_ID": "", "PL_LD_SDK_KEY": ""}),
		config("pl-sentry-dsn-config", map[string]string{"PL_PROD_SENTRY": "", "PL_DEV_SENTRY": "", "PL_OPERATOR_SENTRY": ""}),
		config("segment-config", map[string]string{"write-key": "", "ui-write-key": "", "cli-write-key": ""}),
	}
}

func transformPixieCloudObject(object client.Object, input pixieCloudRender) error {
	cloud := input.Cloud
	switch o := object.(type) {
	case *corev1.ConfigMap:
		return transformPixieCloudConfig(o, input)
	case *appsv1.StatefulSet:
		if o.Name == "pl-nats" {
			// JetStream's volumes are spec.storage.messaging, and its store is capped below them.
			if len(o.Spec.VolumeClaimTemplates) != 1 {
				return fmt.Errorf("StatefulSet pl-nats has %d volume claim templates, want 1", len(o.Spec.VolumeClaimTemplates))
			}
			claim := &o.Spec.VolumeClaimTemplates[0].Spec
			claim.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: quantityOr(cloud.Spec.Storage.Messaging, "10Gi")}
			claim.StorageClassName = storageClassOrNil(cloud.Spec.Storage.StorageClassName)
		}
	}
	spec, _ := podSpecOf(object)
	if spec == nil {
		return nil
	}
	name := object.GetName()
	podTemplateAnnotations(object)[pixieCertsAnnotation] = input.CertsHash
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			c := &containers[i]
			removeEnv(c, "PL_SENTRY_DSN")
			if db, ok := pixieDatabaseServices[name]; ok {
				if usesDatabase(c) {
					setPixieDatabaseEnv(c, db, input.DatabaseHost)
				}
			} else {
				removeEnv(c, "PL_POSTGRES_USERNAME")
				removeEnv(c, "PL_POSTGRES_PASSWORD")
				removeEnv(c, "PL_DATABASE_KEY")
				c.EnvFrom = withoutEnvFrom(c.EnvFrom, "pl-db-config")
			}
		}
	}
	switch name {
	case "api-server":
		app := containerNamed(spec, "api-server")
		setEnv(app, "PL_ELASTIC_USERNAME", "pixie-api")
		setEnvVar(app, secretEnv("PL_ELASTIC_PASSWORD", pixieSearchSecret, "api-password"))
		setEnv(app, "PL_ELASTIC_CA_CERT", "/certs/ca.crt")
		removeEnv(app, "PL_VIZIER_IMAGE_SECRET_PATH")
		removeEnv(app, "PL_VIZIER_IMAGE_SECRET_FILE")
		dropVolume(spec, "vizier-image-secret")
		dropVolume(spec, "elastic-certs-pub")
		// The API server reads the bundle itself (the build patch 0001-api-script-bundle-urls): from the bundle server in
		// this namespace, since nothing in the cluster resolves the hub's domain.
		setEnv(app, "SCRIPT_BUNDLE_URLS", pixieScriptBundleInCluster())
	case "indexer-server":
		app := containerNamed(spec, "indexer-server")
		setEnv(app, "PL_ES_URL", fmt.Sprintf("https://%s.%s.svc:%d", pixieSearchService, oramav1alpha1.PixieCloudNamespace, pixieSearchPort))
		setEnv(app, "PL_ES_USER", "pixie-indexer")
		setEnvVar(app, secretEnv("PL_ES_PASSWD", pixieSearchSecret, "indexer-password"))
		setEnv(app, "PL_ES_CA_CERT", "/certs/ca.crt")
		dropVolume(spec, "es-certs")
	case "artifact-tracker-server":
		// Versions are pinned to the release, so the tracker never polls the public manifest, and
		// no Google credentials are mounted.
		app := containerNamed(spec, "artifact-tracker-server")
		setEnv(app, "PL_VIZIER_VERSION", cloud.Spec.Release.VizierVersion)
		setEnv(app, "PL_CLI_VERSION", cloud.Spec.Release.CLIVersion)
		setEnv(app, "PL_OPERATOR_VERSION", cloud.Spec.Release.VizierVersion)
		dropVolume(spec, "artifact-access-sa")
	case "scriptmgr-server":
		// The script manager reads its bundle through Google's storage client (v1.10.0), which
		// sends object reads to http://$STORAGE_EMULATOR_HOST/<bucket>/<object> (host:port, no
		// scheme): the bundle server in this namespace. Its once-a-minute refresh asks Google's
		// metadata API, which an air-gapped hub cannot reach, so the bundle is read at start and the
		// pod rolls when the bundle image changes.
		app := containerNamed(spec, "scriptmgr-server")
		setEnv(app, "STORAGE_EMULATOR_HOST", fmt.Sprintf("%s.%s.svc:%d", pixieScriptsService, oramav1alpha1.PixieCloudNamespace, pixieScriptsPort))
		podTemplateAnnotations(object)["orama.cloud/pixie-script-bundle"] = string(cloud.Spec.Release.Images["script-bundle"])
	}
	pruneUnmountedVolumes(spec)
	return nil
}

func usesDatabase(c *corev1.Container) bool {
	for _, env := range c.Env {
		if env.Name == "PL_POSTGRES_USERNAME" || env.Name == "PL_POSTGRES_PASSWORD" {
			return true
		}
	}
	for _, from := range c.EnvFrom {
		if from.ConfigMapRef != nil && from.ConfigMapRef.Name == "pl-db-config" {
			return true
		}
	}
	return false
}

// setPixieDatabaseEnv gives one container its service's own database, role and encryption key.
// Explicit env takes precedence over pl-db-config's values, which the DSNs of Hydra and Kratos
// also expand.
func setPixieDatabaseEnv(c *corev1.Container, database, host string) {
	secret := pixieDatabaseSecret(database)
	setEnv(c, "PL_POSTGRES_HOSTNAME", host)
	setEnv(c, "PL_POSTGRES_PORT", "5432")
	setEnv(c, "PL_POSTGRES_DB", database)
	setEnvVar(c, secretEnv("PL_POSTGRES_USERNAME", secret, corev1.BasicAuthUsernameKey))
	setEnvVar(c, secretEnv("PL_POSTGRES_PASSWORD", secret, corev1.BasicAuthPasswordKey))
	for _, env := range c.Env {
		if env.Name == "PL_DATABASE_KEY" {
			setEnvVar(c, secretEnv("PL_DATABASE_KEY", secret, "database-key"))
			break
		}
	}
	// Re-order so the variables a DSN expands are defined before it.
	reorderEnvFirst(c, "PL_POSTGRES_HOSTNAME", "PL_POSTGRES_PORT", "PL_POSTGRES_DB", "PL_POSTGRES_USERNAME", "PL_POSTGRES_PASSWORD")
}

func reorderEnvFirst(c *corev1.Container, names ...string) {
	first := make([]corev1.EnvVar, 0, len(c.Env))
	rest := make([]corev1.EnvVar, 0, len(c.Env))
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for _, name := range names {
		for _, env := range c.Env {
			if env.Name == name {
				first = append(first, env)
			}
		}
	}
	for _, env := range c.Env {
		if !wanted[env.Name] {
			rest = append(rest, env)
		}
	}
	c.Env = append(first, rest...)
}

func withoutEnvFrom(sources []corev1.EnvFromSource, configMap string) []corev1.EnvFromSource {
	kept := sources[:0]
	for _, source := range sources {
		if source.ConfigMapRef == nil || source.ConfigMapRef.Name != configMap {
			kept = append(kept, source)
		}
	}
	return kept
}

func dropVolume(spec *corev1.PodSpec, name string) {
	for i := range spec.Containers {
		mounts := spec.Containers[i].VolumeMounts[:0]
		for _, mount := range spec.Containers[i].VolumeMounts {
			if mount.Name != name {
				mounts = append(mounts, mount)
			}
		}
		spec.Containers[i].VolumeMounts = mounts
	}
	kept := spec.Volumes[:0]
	for _, volume := range spec.Volumes {
		if volume.Name != name {
			kept = append(kept, volume)
		}
	}
	spec.Volumes = kept
}

// pixieScriptBundlePath is where the UI fetches its PxL script bundle: the proxy's /pxl_scripts/
// location (pixieScriptsLocation), on the page's own origin.
const pixieScriptBundlePath = "/pxl_scripts/bundle.json"

// pixieScriptBundleInCluster is the API server's SCRIPT_BUNDLE_URLS: the bundle server's Service.
func pixieScriptBundleInCluster() string {
	urls, _ := json.Marshal([]string{fmt.Sprintf("http://%s.%s.svc:%d/pixie-scripts/bundle.json", pixieScriptsService, oramav1alpha1.PixieCloudNamespace, pixieScriptsPort)})
	return string(urls)
}

// pixieScriptsLocation is appended to the proxy's API locations: the UI fetches its PxL script
// bundle from the hub's own domain instead of artifacts.px.dev.
const pixieScriptsLocation = `
# Added by the Orama operator: the PxL script bundle is served inside the cluster (air-gapped).
location /pxl_scripts/ {
    proxy_pass http://pixie-scripts.plc.svc.cluster.local:8080/pixie-scripts/;
}
`

func transformPixieCloudConfig(cm *corev1.ConfigMap, input pixieCloudRender) error {
	cloud := input.Cloud
	set := func(key, value string) error {
		if _, ok := cm.Data[key]; !ok {
			return fmt.Errorf("ConfigMap %s has no %s", cm.Name, key)
		}
		cm.Data[key] = value
		return nil
	}
	switch cm.Name {
	case pixieDomainConfig:
		return set("PL_DOMAIN_NAME", cloud.Spec.Domain)
	case "pl-db-config":
		// Shared defaults only; every service overrides the database, role and host it uses.
		if err := set("PL_POSTGRES_HOSTNAME", input.DatabaseHost); err != nil {
			return err
		}
		return set("PL_POSTGRES_DB", "unused")
	case pixieServiceConfig:
		return set("PL_ELASTIC_SERVICE", fmt.Sprintf("https://%s.%s.svc:%d", pixieSearchService, oramav1alpha1.PixieCloudNamespace, pixieSearchPort))
	case "pl-indexer-config":
		for key, value := range map[string]string{
			"PL_MD_INDEX_NAME":              pixieMetadataIndex,
			"PL_MD_INDEX_REPLICAS":          "0",
			"PL_MD_MANUAL_INDEX_MANAGEMENT": "true",
		} {
			if err := set(key, value); err != nil {
				return err
			}
		}
	case "pl-artifact-config":
		if err := set("PL_ARTIFACT_MANIFEST_URL", ""); err != nil {
			return err
		}
		return set("PL_ARTIFACT_BUCKET", "")
	case "pl-script-bundles-config":
		// What the UI is given (the proxy writes it into the page). A path on the page's own origin:
		// the UI runs on work.<domain>, and a bundle on <domain> is another origin, which the browser
		// refuses without CORS headers ("Failed to Load Script Bundles", Network Error). The API server
		// gets the in-cluster address instead (pixieScriptBundleInCluster).
		urls, _ := json.Marshal([]string{pixieScriptBundlePath})
		return set("SCRIPT_BUNDLE_URLS", string(urls))
	case "proxy-envoy-config":
		// Envoy serves gRPC-web (script runs) on the proxy's port 4444 and answers the browser's CORS
		// preflight. Upstream allows only its own domain, so every script run from our UI was refused
		// (403 to OPTIONS ExecuteScript, "http status code: 0" in the UI). The UI's origin is
		// work.<domain>; the suffix <domain> covers it and the domain itself.
		const upstream = `- suffix: "dev.withpixie.dev"`
		for key, value := range cm.Data {
			if strings.Contains(value, upstream) {
				cm.Data[key] = strings.ReplaceAll(value, upstream, fmt.Sprintf("- suffix: %q", cloud.Spec.Domain))
				return nil
			}
		}
		return fmt.Errorf("ConfigMap %s has no CORS origin %s to replace", cm.Name, upstream)
	case "pl-scriptmgr-config":
		if err := set("PL_BUNDLE_BUCKET", pixieScriptsBucket); err != nil {
			return err
		}
		return set("PL_BUNDLE_PATH", "bundle.json")
	case "pl-analytics-config":
		return set("ANALYTICS_ENABLED", "false")
	case "pl-contact-config":
		return set("CONTACT_ENABLED", "false")
	case "pl-support-access-config":
		return set("PL_SUPPORT_ACCESS_ENABLED", "false")
	case "nats-config":
		// The binary's own limits from the same numbers as the pod's (principle 8): a quarter of
		// the memory limit for in-memory streams, 90% of the volume for files.
		conf, ok := cm.Data["nats.conf"]
		const storeDir = "  store_dir: /data/jetstream\n"
		if !ok || !strings.Contains(conf, storeDir) {
			return fmt.Errorf("ConfigMap nats-config has no JetStream store_dir to cap")
		}
		memoryLimit := resource.MustParse(pixieCloudBudgets["pl-nats/pl-nats"].MemoryLimit)
		volume := quantityOr(cloud.Spec.Storage.Messaging, "10Gi")
		cm.Data["nats.conf"] = strings.Replace(conf, storeDir, fmt.Sprintf("%s  max_memory_store: %d\n  max_file_store: %d\n",
			storeDir, memoryLimit.Value()/4, volume.Value()/10*9), 1)
	case pixieProxyNginxConfig:
		api, ok := cm.Data["pixie_api.conf"]
		if !ok {
			return fmt.Errorf("ConfigMap %s has no pixie_api.conf", cm.Name)
		}
		if !strings.Contains(api, "location /pxl_scripts/") {
			cm.Data["pixie_api.conf"] = api + pixieScriptsLocation
		}
	}
	return nil
}

// pixieDatabaseSecret holds one service's role (username, password) and its encryption key.
func pixieDatabaseSecret(database string) string { return "pixie-db-" + database }

// pixieDatabases is every database the hub's Postgres holds, sorted.
func pixieDatabases() []string {
	seen := map[string]bool{}
	var databases []string
	for _, db := range pixieDatabaseServices {
		if !seen[db] {
			seen[db] = true
			databases = append(databases, db)
		}
	}
	sort.Strings(databases)
	return databases
}

// quantityOr is q, or fallback when q is unset or zero.
func quantityOr(q *resource.Quantity, fallback string) resource.Quantity {
	if q == nil || q.IsZero() {
		return resource.MustParse(fallback)
	}
	return q.DeepCopy()
}
