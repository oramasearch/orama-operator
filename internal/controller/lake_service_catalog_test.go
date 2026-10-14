/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// catalogCase is one catalog kind's fixture: how its service is built, made ready, and
// connected. Every test below runs over all six kinds.
type catalogCase struct {
	kind string
	id   string
	slug string
	// group is the API group whose absence means the kind is not installed; "" for a kind the
	// Orama operator runs itself on built-in resources.
	group string
	// objects are the upstream objects the service writes, in deletion order: Kind/name.
	objects []string
	// checkBuild asserts the built objects for instances 3 and storage_gi 15.
	checkBuild func(h *serviceHarness, c catalogCase)
	// waiting is the Provisioning message before the upstream operator reports ready.
	waiting string
	// markPartlyReady leaves the service one condition short of ready; partWaiting is then the
	// message. markReady completes it.
	markPartlyReady func(h *serviceHarness, c catalogCase)
	partWaiting     string
	markReady       func(h *serviceHarness, c catalogCase)
	// upstreamSecret creates the Secret the upstream operator writes, for kinds whose credential
	// comes from one; nil for kinds whose password the Orama operator generates.
	upstreamSecret func(h *serviceHarness, c catalogCase)
	// passwordSecret is the generated password Secret and key, "" when upstream makes it.
	passwordSecret, passwordKey string
	endpoint                    string
	// credential is the expected <slug>.json for password.
	credential func(password string) string
	// claimLabels are the labels the upstream operator puts on the service's PVCs.
	claimLabels map[string]string
}

func (c catalogCase) name() string { return serviceResourceName("prod", c.slug) }

// spec is the service spec a test asks for, with instances pinned to 1 for SQLite and DuckDB, which
// have one writer.
func (c catalogCase) spec(spec string) string {
	if c.kind == serviceKindSQLite || c.kind == serviceKindDuckDB {
		return strings.Replace(spec, `"instances":3`, `"instances":1`, 1)
	}
	return spec
}

func catalogCases() []catalogCase {
	return []catalogCase{
		{
			kind: serviceKindKafka, id: "66666666-6666-4666-8666-666666666666", slug: "svc-666666666666", group: "kafka.strimzi.io",
			objects: []string{"Kafka/prod-svc-666666666666", "KafkaNodePool/prod-svc-666666666666-pool", "KafkaUser/prod-svc-666666666666-app"},
			checkBuild: func(h *serviceHarness, c catalogCase) {
				kafka := h.upstream(strimziKafkaGVK, c.name())
				listeners, _, _ := unstructured.NestedSlice(kafka.Object, "spec", "kafka", "listeners")
				listener := listeners[0].(map[string]interface{})
				if len(listeners) != 1 || listener["port"] != int64(9092) || listener["type"] != "internal" || listener["tls"] != false ||
					listener["authentication"].(map[string]interface{})["type"] != "scram-sha-512" {
					h.t.Fatalf("listeners = %v", listeners)
				}
				rf, _, _ := unstructured.NestedInt64(kafka.Object, "spec", "kafka", "config", "offsets.topic.replication.factor")
				isr, _, _ := unstructured.NestedInt64(kafka.Object, "spec", "kafka", "config", "min.insync.replicas")
				if _, found, _ := unstructured.NestedMap(kafka.Object, "spec", "entityOperator", "userOperator"); !found || rf != 3 || isr != 2 {
					h.t.Fatalf("kafka spec = %v", kafka.Object["spec"])
				}
				pool := h.upstream(strimziNodePoolGVK, c.name()+"-pool")
				replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
				roles, _, _ := unstructured.NestedStringSlice(pool.Object, "spec", "roles")
				size, _, _ := unstructured.NestedString(pool.Object, "spec", "storage", "size")
				kind, _, _ := unstructured.NestedString(pool.Object, "spec", "storage", "type")
				if replicas != 3 || fmt.Sprint(roles) != "[controller broker]" || size != "15Gi" || kind != "persistent-claim" ||
					pool.GetLabels()[strimziClusterLabel] != c.name() {
					h.t.Fatalf("node pool = %v %v", pool.GetLabels(), pool.Object["spec"])
				}
				user := h.upstream(strimziKafkaUserGVK, c.name()+"-app")
				auth, _, _ := unstructured.NestedString(user.Object, "spec", "authentication", "type")
				if auth != "scram-sha-512" || user.GetLabels()[strimziClusterLabel] != c.name() {
					h.t.Fatalf("kafka user = %v %v", user.GetLabels(), user.Object["spec"])
				}
			},
			waiting: "waiting for the Kafka cluster",
			markReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(strimziKafkaGVK, c.name(), map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}})
				h.setStatus(strimziKafkaUserGVK, c.name()+"-app", map[string]interface{}{"username": c.name() + "-app", "secret": c.name() + "-app"})
			},
			upstreamSecret: func(h *serviceHarness, c catalogCase) {
				user := h.upstream(strimziKafkaUserGVK, c.name()+"-app")
				h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.name() + "-app", Namespace: testServiceNamespace,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "kafka.strimzi.io/v1", Kind: "KafkaUser", Name: user.GetName(), UID: user.GetUID()}}},
					Data: map[string][]byte{"password": []byte("scram-secret"), "sasl.jaas.config": []byte("...")}})
			},
			endpoint: "prod-svc-666666666666-kafka-bootstrap.lake.svc.cluster.local:9092",
			credential: func(string) string {
				return `{"kind":"kafka","endpoint":"prod-svc-666666666666-kafka-bootstrap.lake.svc.cluster.local:9092",` +
					`"bootstrap":"prod-svc-666666666666-kafka-bootstrap.lake.svc.cluster.local:9092","username":"prod-svc-666666666666-app",` +
					`"password":"scram-secret","sasl_mechanism":"SCRAM-SHA-512","security_protocol":"SASL_PLAINTEXT"}`
			},
			claimLabels: map[string]string{strimziClusterLabel: "prod-svc-666666666666", strimziKindLabel: "Kafka"},
		},
		{
			kind: serviceKindClickHouse, id: "77777777-7777-4777-8777-777777777777", slug: "svc-777777777777", group: "clickhouse.altinity.com",
			objects: []string{"ClickHouseInstallation/prod-svc-777777777777", "ClickHouseKeeperInstallation/prod-svc-777777777777"},
			checkBuild: func(h *serviceHarness, c catalogCase) {
				chi := h.upstream(clickHouseInstallationGVK, c.name())
				clusters, _, _ := unstructured.NestedSlice(chi.Object, "spec", "configuration", "clusters")
				layout := clusters[0].(map[string]interface{})["layout"].(map[string]interface{})
				templates, _, _ := unstructured.NestedSlice(chi.Object, "spec", "templates", "volumeClaimTemplates")
				size, _, _ := unstructured.NestedString(templates[0].(map[string]interface{}), "spec", "resources", "requests", "storage")
				keeper, _, _ := unstructured.NestedString(chi.Object, "spec", "configuration", "zookeeper", "keeper", "name")
				template, _, _ := unstructured.NestedString(chi.Object, "spec", "defaults", "templates", "dataVolumeClaimTemplate")
				if layout["shardsCount"] != int64(1) || layout["replicasCount"] != int64(3) || size != "15Gi" || keeper != c.name() || template != "data" {
					h.t.Fatalf("installation spec = %v", chi.Object["spec"])
				}
				password := string(h.secret(c.name() + "-credentials").Data["password"])
				digest := sha256.Sum256([]byte(password))
				users, _, _ := unstructured.NestedMap(chi.Object, "spec", "configuration", "users")
				if len(password) != 32 || users["app/password_sha256_hex"] != hex.EncodeToString(digest[:]) || strings.Contains(fmt.Sprint(chi.Object), password) {
					h.t.Fatalf("users = %v (password %q)", users, password)
				}
				chk := h.upstream(clickHouseKeeperGVK, c.name())
				keeperClusters, _, _ := unstructured.NestedSlice(chk.Object, "spec", "configuration", "clusters")
				if replicas := keeperClusters[0].(map[string]interface{})["layout"].(map[string]interface{})["replicasCount"]; replicas != int64(1) {
					h.t.Fatalf("keeper spec = %v", chk.Object["spec"])
				}
			},
			waiting: "waiting for the ClickHouse installation",
			markPartlyReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(clickHouseInstallationGVK, c.name(), map[string]interface{}{"status": "Completed"})
				h.setStatus(clickHouseKeeperGVK, c.name(), map[string]interface{}{"status": "InProgress"})
			},
			partWaiting: "waiting for the ClickHouse Keeper",
			markReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(clickHouseKeeperGVK, c.name(), map[string]interface{}{"status": "Completed"})
			},
			passwordSecret: "prod-svc-777777777777-credentials", passwordKey: "password",
			endpoint: "clickhouse-prod-svc-777777777777.lake.svc.cluster.local:8123",
			credential: func(password string) string {
				return `{"kind":"clickhouse","endpoint":"clickhouse-prod-svc-777777777777.lake.svc.cluster.local:8123",` +
					`"host":"clickhouse-prod-svc-777777777777.lake.svc.cluster.local","http_port":8123,"native_port":9000,` +
					`"user":"app","password":"` + password + `","database":"default"}`
			},
			claimLabels: map[string]string{clickHouseInstallationLabel: "prod-svc-777777777777", clickHouseAppLabel: "chop"},
		},
		{
			kind: serviceKindTiDB, id: "88888888-8888-4888-8888-888888888888", slug: "svc-888888888888", group: "pingcap.com",
			objects: []string{"TidbCluster/prod-svc-888888888888"},
			checkBuild: func(h *serviceHarness, c catalogCase) {
				tc := h.upstream(tidbClusterGVK, c.name())
				for _, component := range tidbComponents {
					replicas, _, _ := unstructured.NestedInt64(tc.Object, "spec", component, "replicas")
					failover, found, _ := unstructured.NestedInt64(tc.Object, "spec", component, "maxFailoverCount")
					if replicas != 3 || !found || failover != 0 {
						h.t.Fatalf("%s spec = %v", component, tc.Object["spec"])
					}
				}
				for _, component := range []string{"pd", "tikv"} {
					if size, _, _ := unstructured.NestedString(tc.Object, "spec", component, "requests", "storage"); size != "15Gi" {
						h.t.Fatalf("%s storage = %q", component, size)
					}
				}
				version, _, _ := unstructured.NestedString(tc.Object, "spec", "version")
				create, _, _ := unstructured.NestedBool(tc.Object, "spec", "tidb", "initializer", "createPassword")
				service, _, _ := unstructured.NestedString(tc.Object, "spec", "tidb", "service", "type")
				maxReplicas, _, _ := unstructured.NestedInt64(tc.Object, "spec", "pd", "config", "replication", "max-replicas")
				if version != tidbVersion || !create || service != "ClusterIP" || maxReplicas != 3 || tc.GetLabels()["app.kubernetes.io/instance"] != c.name() {
					h.t.Fatalf("tidb cluster = %v %v", tc.GetLabels(), tc.Object["spec"])
				}
				if len(h.secret(c.name() + "-init").Data["root"]) != 32 {
					h.t.Fatal("no generated root password in <cluster>-init")
				}
			},
			waiting: "waiting for TiDB pd",
			markPartlyReady: func(h *serviceHarness, c catalogCase) {
				status := map[string]interface{}{}
				for _, component := range tidbComponents {
					status[component] = map[string]interface{}{"phase": "Normal", "statefulSet": map[string]interface{}{"replicas": int64(3), "readyReplicas": int64(3)}}
				}
				h.setStatus(tidbClusterGVK, c.name(), status)
			},
			partWaiting: "waiting for the TiDB root password",
			markReady: func(h *serviceHarness, c catalogCase) {
				tc := h.upstream(tidbClusterGVK, c.name())
				if err := unstructured.SetNestedField(tc.Object, true, "status", "tidb", "passwordInitialized"); err != nil {
					h.t.Fatal(err)
				}
				h.update(tc)
			},
			passwordSecret: "prod-svc-888888888888-init", passwordKey: "root",
			endpoint: "prod-svc-888888888888-tidb.lake.svc.cluster.local:4000",
			credential: func(password string) string {
				return `{"kind":"tidb","endpoint":"prod-svc-888888888888-tidb.lake.svc.cluster.local:4000",` +
					`"host":"prod-svc-888888888888-tidb.lake.svc.cluster.local","port":4000,"user":"root","password":"` + password + `",` +
					`"uri":"mysql://root:` + password + `@prod-svc-888888888888-tidb.lake.svc.cluster.local:4000"}`
			},
			claimLabels: map[string]string{"app.kubernetes.io/instance": "prod-svc-888888888888", "app.kubernetes.io/managed-by": "tidb-operator"},
		},
		{
			kind: serviceKindCassandra, id: "99999999-9999-4999-8999-999999999999", slug: "svc-999999999999", group: "k8ssandra.io",
			objects: []string{"K8ssandraCluster/prod-svc-999999999999"},
			checkBuild: func(h *serviceHarness, c catalogCase) {
				kc := h.upstream(k8ssandraClusterGVK, c.name())
				datacenters, _, _ := unstructured.NestedSlice(kc.Object, "spec", "cassandra", "datacenters")
				dc := datacenters[0].(map[string]interface{})
				size, _, _ := unstructured.NestedString(dc, "storageConfig", "cassandraDataVolumeClaimSpec", "resources", "requests", "storage")
				name, _, _ := unstructured.NestedString(dc, "metadata", "name")
				cluster, _, _ := unstructured.NestedString(kc.Object, "spec", "cassandra", "clusterName")
				version, _, _ := unstructured.NestedString(kc.Object, "spec", "cassandra", "serverVersion")
				serverType, _, _ := unstructured.NestedString(kc.Object, "spec", "cassandra", "serverType")
				if len(datacenters) != 1 || dc["size"] != int64(3) || size != "15Gi" || name != c.slug || dc["datacenterName"] != "dc1" ||
					cluster != c.slug || version != cassandraVersion || serverType != "cassandra" {
					h.t.Fatalf("k8ssandra cluster spec = %v", kc.Object["spec"])
				}
			},
			waiting: "waiting for the Cassandra datacenter",
			markPartlyReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(k8ssandraClusterGVK, c.name(), map[string]interface{}{"datacenters": map[string]interface{}{c.slug: map[string]interface{}{
					"cassandra": map[string]interface{}{"cassandraOperatorProgress": "Updating",
						"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}}}}})
			},
			partWaiting: "waiting for the Cassandra datacenter",
			markReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(k8ssandraClusterGVK, c.name(), map[string]interface{}{"datacenters": map[string]interface{}{c.slug: map[string]interface{}{
					"cassandra": map[string]interface{}{"cassandraOperatorProgress": "Ready",
						"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}}}}})
			},
			upstreamSecret: func(h *serviceHarness, c catalogCase) {
				kc := h.upstream(k8ssandraClusterGVK, c.name())
				controller := true
				h.create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.slug + "-superuser", Namespace: testServiceNamespace,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "k8ssandra.io/v1alpha1", Kind: "K8ssandraCluster", Name: kc.GetName(), UID: kc.GetUID(), Controller: &controller}}},
					Data: map[string][]byte{"username": []byte(c.slug + "-superuser"), "password": []byte("cql-secret")}})
			},
			endpoint: "svc-999999999999-svc-999999999999-service.lake.svc.cluster.local:9042",
			credential: func(string) string {
				return `{"kind":"cassandra","endpoint":"svc-999999999999-svc-999999999999-service.lake.svc.cluster.local:9042",` +
					`"host":"svc-999999999999-svc-999999999999-service.lake.svc.cluster.local","port":9042,"datacenter":"dc1",` +
					`"user":"svc-999999999999-superuser","password":"cql-secret"}`
			},
			claimLabels: map[string]string{cassandraClusterLabel: "svc-999999999999"},
		},
		{
			kind: serviceKindValkey, id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", slug: "svc-aaaaaaaaaaaa", group: "valkey.io",
			objects: []string{"ValkeyCluster/prod-svc-aaaaaaaaaaaa"},
			checkBuild: func(h *serviceHarness, c catalogCase) {
				vc := h.upstream(valkeyClusterGVK, c.name())
				shards, _, _ := unstructured.NestedInt64(vc.Object, "spec", "shards")
				replicas, _, _ := unstructured.NestedInt64(vc.Object, "spec", "replicas")
				size, _, _ := unstructured.NestedString(vc.Object, "spec", "persistence", "size")
				users, _, _ := unstructured.NestedSlice(vc.Object, "spec", "users")
				defaultUser, app := users[0].(map[string]interface{}), users[1].(map[string]interface{})
				secret, _, _ := unstructured.NestedString(app, "passwordSecret", "name")
				if shards != 3 || replicas != 2 || size != "15Gi" || defaultUser["name"] != "default" || defaultUser["enabled"] != false ||
					app["name"] != "app" || secret != c.name()+"-credentials" {
					h.t.Fatalf("valkey cluster spec = %v", vc.Object["spec"])
				}
			},
			waiting: "waiting for the Valkey cluster",
			markPartlyReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(valkeyClusterGVK, c.name(), map[string]interface{}{"state": "Reconciling",
					"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "False"}}})
			},
			partWaiting: "waiting for the Valkey cluster",
			markReady: func(h *serviceHarness, c catalogCase) {
				h.setStatus(valkeyClusterGVK, c.name(), map[string]interface{}{"state": "Ready",
					"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}})
			},
			passwordSecret: "prod-svc-aaaaaaaaaaaa-credentials", passwordKey: "password",
			endpoint: "valkey-prod-svc-aaaaaaaaaaaa.lake.svc.cluster.local:6379",
			credential: func(password string) string {
				return `{"kind":"valkey","endpoint":"valkey-prod-svc-aaaaaaaaaaaa.lake.svc.cluster.local:6379",` +
					`"host":"valkey-prod-svc-aaaaaaaaaaaa.lake.svc.cluster.local","port":6379,"user":"app","password":"` + password + `","cluster":true}`
			},
			claimLabels: map[string]string{valkeyClusterLabel: "prod-svc-aaaaaaaaaaaa", "app.kubernetes.io/managed-by": "valkey-operator"},
		},
		sqliteCatalogCase(),
		duckdbCatalogCase(),
		polarsCatalogCase(),
	}
}

func catalogGVK(kind string) schema.GroupVersionKind {
	for _, gvk := range []schema.GroupVersionKind{strimziKafkaGVK, strimziNodePoolGVK, strimziKafkaUserGVK, clickHouseInstallationGVK,
		clickHouseKeeperGVK, tidbClusterGVK, k8ssandraClusterGVK, valkeyClusterGVK, statefulSetGVK, serviceGVK} {
		if gvk.Kind == kind {
			return gvk
		}
	}
	panic("no GVK for " + kind)
}

func (h *serviceHarness) upstream(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	h.t.Helper()
	obj := newServiceObject(gvk, testServiceNamespace, name)
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		h.t.Fatalf("get %s %s: %v", gvk.Kind, name, err)
	}
	return obj
}

func (h *serviceHarness) exists(gvk schema.GroupVersionKind, name string) bool {
	h.t.Helper()
	err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: name}, newServiceObject(gvk, testServiceNamespace, name))
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("get %s %s: %v", gvk.Kind, name, err)
	}
	return err == nil
}

func (h *serviceHarness) update(obj client.Object) {
	h.t.Helper()
	if err := h.client.Update(context.Background(), obj); err != nil {
		h.t.Fatalf("update %s: %v", obj.GetName(), err)
	}
}

// setStatus merges fields into an upstream object's status, as its operator would. A StatefulSet's
// status is a subresource, as it is on the API server; the fake client serves the upstream CRDs
// without one.
func (h *serviceHarness) setStatus(gvk schema.GroupVersionKind, name string, fields map[string]interface{}) {
	h.t.Helper()
	obj := h.upstream(gvk, name)
	for key, value := range fields {
		if err := unstructured.SetNestedField(obj.Object, value, "status", key); err != nil {
			h.t.Fatal(err)
		}
	}
	if gvk == statefulSetGVK {
		if err := h.client.Status().Update(context.Background(), obj); err != nil {
			h.t.Fatalf("update status of %s: %v", name, err)
		}
		return
	}
	h.update(obj)
}

func (h *serviceHarness) secret(name string) *corev1.Secret {
	h.t.Helper()
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: name}, secret); err != nil {
		h.t.Fatalf("get secret %s: %v", name, err)
	}
	return secret
}

// giveUIDs stamps a UID on every upstream object, as the API server does; the fake client does
// not, and owner references of upstream Secrets are matched by UID.
func (h *serviceHarness) giveUIDs(c catalogCase) {
	h.t.Helper()
	for i, ref := range c.objects {
		kind, name, _ := strings.Cut(ref, "/")
		obj := h.upstream(catalogGVK(kind), name)
		obj.SetUID(types.UID(fmt.Sprintf("%s-uid-%d", c.kind, i)))
		h.update(obj)
	}
}

func (h *serviceHarness) password(c catalogCase) string {
	if c.passwordSecret == "" {
		return ""
	}
	return string(h.secret(c.passwordSecret).Data[c.passwordKey])
}

func catalogClaim(c catalogCase, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testServiceNamespace, Labels: c.claimLabels}}
}

// readyCatalogService brings c's service to Ready and returns the harness and store.
func readyCatalogService(t *testing.T, c catalogCase) (*serviceHarness, *fakeServiceStore) {
	t.Helper()
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.giveUIDs(c)
	if c.markPartlyReady != nil {
		c.markPartlyReady(h, c)
	}
	c.markReady(h, c)
	if c.upstreamSecret != nil {
		c.upstreamSecret(h, c)
	}
	h.poll()
	if status := store.status(t, c.id); status.Phase != ServicePhaseReady {
		t.Fatalf("service not Ready: %+v", status)
	}
	return h, store
}

func TestServicesCatalogBuild(t *testing.T) {
	for _, c := range catalogCases() {
		t.Run(c.kind, func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			for _, ref := range c.objects {
				kind, name, _ := strings.Cut(ref, "/")
				obj := h.upstream(catalogGVK(kind), name)
				if !metav1.IsControlledBy(obj, h.lake) {
					t.Fatalf("%s is not controlled by the Lake", ref)
				}
				for label, want := range map[string]string{serviceLakeLabel: "prod", serviceIDLabel: c.id, serviceKindLabel: c.kind} {
					if obj.GetLabels()[label] != want {
						t.Errorf("%s label %s = %q, want %q", ref, label, obj.GetLabels()[label], want)
					}
				}
				if obj.GetAnnotations()[serviceCustomerAnnotation] != testServiceCustomer {
					t.Errorf("%s customer annotation = %q", ref, obj.GetAnnotations()[serviceCustomerAnnotation])
				}
			}
			c.checkBuild(h, c)
			if c.passwordSecret != "" {
				secret := h.secret(c.passwordSecret)
				if !metav1.IsControlledBy(secret, h.lake) || secret.Labels[serviceIDLabel] != c.id {
					t.Fatalf("password Secret not the Lake's: %v %v", secret.OwnerReferences, secret.Labels)
				}
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseProvisioning, c.waiting, c.endpoint)

			// A steady service is never rewritten, and its password never rotates.
			password := h.password(c)
			*h.updates = 0
			h.poll()
			h.poll()
			if *h.updates != 0 {
				t.Fatalf("steady passes issued %d updates", *h.updates)
			}
			if h.password(c) != password {
				t.Fatal("the generated password changed")
			}
		})
	}
}

func TestServicesCatalogReadyGatingAndCredential(t *testing.T) {
	for _, c := range catalogCases() {
		t.Run(c.kind, func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3,"storage_gi":15}`), false)}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			h.giveUIDs(c)
			expectStatus(t, store.status(t, c.id), ServicePhaseProvisioning, c.waiting, c.endpoint)

			if c.markPartlyReady != nil {
				c.markPartlyReady(h, c)
				h.poll()
				expectStatus(t, store.status(t, c.id), ServicePhaseProvisioning, c.partWaiting, c.endpoint)
			}
			c.markReady(h, c)
			h.poll()
			if c.upstreamSecret != nil {
				// Upstream ready, but its Secret is not there yet: not Ready, no key.
				expectStatus(t, store.status(t, c.id), ServicePhaseProvisioning, "delivering the connection", c.endpoint)
				if _, found := h.credentials()[c.slug+".json"]; found {
					t.Fatal("credential written before the upstream Secret exists")
				}
				c.upstreamSecret(h, c)
				h.poll()
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
			if got, want := string(h.credentials()[c.slug+".json"]), c.credential(h.password(c)); got != want {
				t.Fatalf("credential =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestServicesCatalogDeletionOrder(t *testing.T) {
	for _, c := range catalogCases() {
		t.Run(c.kind, func(t *testing.T) {
			h, store := readyCatalogService(t, c)
			h.create(catalogClaim(c, "data-"+c.name()+"-0"))
			h.create(catalogClaim(c, "data-"+c.name()+"-1"))
			unrelated := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-other", Namespace: testServiceNamespace,
				Labels: map[string]string{"app": "other"}}}
			h.create(unrelated)

			// Hold the primary with a finalizer, as its upstream operator does, so the first pass
			// sees it still there.
			kind, name, _ := strings.Cut(c.objects[0], "/")
			primary := h.upstream(catalogGVK(kind), name)
			primary.SetFinalizers([]string{"test/hold"})
			h.update(primary)
			store.setDesired(c.id, serviceDesiredAbsent)
			h.poll()
			if fmt.Sprint(*h.deletedKinds) != fmt.Sprint(c.objects) {
				t.Fatalf("first pass deleted %v, want the upstream objects %v", *h.deletedKinds, c.objects)
			}
			if _, found := h.credentials()[c.slug+".json"]; found {
				t.Fatal("credential key kept for an absent service")
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseDeleting, fmt.Sprintf("deleting the %s cluster", catalogKinds[c.kind].title()), c.endpoint)

			// Once the upstream objects are gone: the volumes, then — once they are gone too — the
			// generated password.
			primary = h.upstream(catalogGVK(kind), name)
			primary.SetFinalizers(nil)
			h.update(primary)
			h.poll()
			want := append([]string{}, c.objects...)
			want = append(want, "PersistentVolumeClaim/data-"+c.name()+"-0", "PersistentVolumeClaim/data-"+c.name()+"-1")
			if fmt.Sprint(*h.deletedKinds) != fmt.Sprint(want) {
				t.Fatalf("deletion order = %v, want %v", *h.deletedKinds, want)
			}
			if c.passwordSecret != "" {
				h.secret(c.passwordSecret)
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseDeleting, fmt.Sprintf("deleting the %s data volumes", catalogKinds[c.kind].title()), c.endpoint)

			h.poll()
			if c.passwordSecret != "" {
				want = append(want, "Secret/"+c.passwordSecret)
			}
			if fmt.Sprint(*h.deletedKinds) != fmt.Sprint(want) {
				t.Fatalf("deletion order = %v, want %v", *h.deletedKinds, want)
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseDeleted, "service deleted", "")
			if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &corev1.PersistentVolumeClaim{}); err != nil {
				t.Fatalf("an unrelated volume was touched: %v", err)
			}
		})
	}
}

func TestServicesCatalogRefusesForeignObjects(t *testing.T) {
	for _, c := range catalogCases() {
		t.Run(c.kind, func(t *testing.T) {
			// Every object of the service's names, labelled as the service but not controlled
			// by the Lake: each one is refused, the primary first.
			kind, name, _ := strings.Cut(c.objects[0], "/")
			foreign := newServiceObject(catalogGVK(kind), testServiceNamespace, name)
			foreign.SetLabels(map[string]string{serviceLakeLabel: "prod", serviceIDLabel: c.id})
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3}`), false)}}
			h := newServiceHarness(t, serviceTestLake(), store, foreign)
			h.poll()
			want := fmt.Sprintf("%s lake/%s exists and is not this Lake's service", kind, name)
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, want, "")
			if c.passwordSecret != "" {
				if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: c.passwordSecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatalf("a refused service generated a password Secret: %v", err)
				}
			}

			store.setDesired(c.id, serviceDesiredAbsent)
			h.poll()
			if len(*h.deletes) != 0 {
				t.Fatalf("deleted a foreign object: %v", *h.deletes)
			}
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, want, "")
		})
	}

	// A secondary object held by someone else is refused too.
	c := catalogCases()[0]
	user := newServiceObject(strimziKafkaUserGVK, testServiceNamespace, c.name()+"-app")
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store, user)
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseFailed, "KafkaUser lake/prod-svc-666666666666-app exists and is not this Lake's service", "")
	if h.exists(strimziKafkaGVK, c.name()) {
		t.Fatal("a Kafka was created next to a foreign KafkaUser")
	}
}

func TestServicesCatalogRefusesForeignSecrets(t *testing.T) {
	// A generated password Secret of the service's name the Lake does not own.
	for _, c := range catalogCases() {
		if c.passwordSecret == "" {
			continue
		}
		t.Run(c.kind+"/password", func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{}`, false)}}
			h := newServiceHarness(t, serviceTestLake(), store, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.passwordSecret, Namespace: testServiceNamespace},
				Data: map[string][]byte{c.passwordKey: []byte("chosen-by-someone-else")}})
			h.poll()
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, fmt.Sprintf("Secret lake/%s exists and is not this Lake's service", c.passwordSecret), "")
		})
	}
	// An upstream Secret its upstream object does not own is not trusted as the credential.
	for _, c := range catalogCases() {
		if c.upstreamSecret == nil {
			continue
		}
		t.Run(c.kind+"/upstream", func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, c.spec(`{"instances":3}`), false)}}
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			h.giveUIDs(c)
			c.markReady(h, c)
			c.upstreamSecret(h, c)
			name := c.name() + "-app"
			if c.kind == serviceKindCassandra {
				name = c.slug + "-superuser"
			}
			secret := h.secret(name)
			secret.OwnerReferences = nil
			h.update(secret)
			h.poll()
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, fmt.Sprintf("Secret lake/%s exists and is not this Lake's service", name), c.endpoint)
			if _, found := h.credentials()[c.slug+".json"]; found {
				t.Fatal("an untrusted credential was written")
			}
		})
	}
}

func TestServicesCatalogOperatorNotInstalled(t *testing.T) {
	for _, c := range catalogCases() {
		if c.group == "" {
			continue
		}
		t.Run(c.kind, func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{
				testService(c.id, c.slug, c.kind, `{}`, false),
				testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
			}}
			missing := []string{c.group}
			if c.kind == serviceKindClickHouse {
				missing = append(missing, clickHouseKeeperGVK.Group)
			}
			h := newServiceHarnessWithout(t, serviceTestLake(), store, missing)
			h.poll()
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, fmt.Sprintf("the %s operator is not installed on this cluster", c.kind), "")
			// The rest of the Lake's services carry on.
			expectStatus(t, store.status(t, testPostgresID), ServicePhaseProvisioning, "waiting for the Postgres instances",
				"prod-svc-222222222222-rw.lake.svc.cluster.local:5432")
			if c.passwordSecret != "" {
				if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: c.passwordSecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatalf("a password Secret was generated for a kind that cannot run: %v", err)
				}
			}

			// Deleting it needs nothing from the missing operator.
			store.setDesired(c.id, serviceDesiredAbsent)
			h.poll()
			expectStatus(t, store.status(t, c.id), ServicePhaseDeleted, "service deleted", "")
		})
	}

	// The ClickHouse Keeper CRD alone missing fails the service the same way.
	c := catalogCases()[1]
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{"instances":2}`, false)}}
	h := newServiceHarnessWithout(t, serviceTestLake(), store, []string{clickHouseKeeperGVK.Group})
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseFailed, "the clickhouse operator is not installed on this cluster", "")
}

func TestServicesCatalogOldVolumeBlocksCreation(t *testing.T) {
	for _, c := range catalogCases() {
		t.Run(c.kind, func(t *testing.T) {
			store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{}`, false)}}
			h := newServiceHarness(t, serviceTestLake(), store, catalogClaim(c, "data-old"))
			h.poll()
			expectStatus(t, store.status(t, c.id), ServicePhaseFailed, "PersistentVolumeClaim lake/data-old exists and is not this Lake's service", "")
			kind, name, _ := strings.Cut(c.objects[0], "/")
			if h.exists(catalogGVK(kind), name) {
				t.Fatal("a service was created over an old volume")
			}
		})
	}
}

func TestServicesClickHouseSingleReplicaHasNoKeeper(t *testing.T) {
	c := catalogCases()[1]
	store := &fakeServiceStore{services: []LakeService{testService(c.id, c.slug, c.kind, `{"instances":1}`, false)}}
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	if h.exists(clickHouseKeeperGVK, c.name()) {
		t.Fatal("a single-replica installation got a Keeper")
	}
	if _, found, _ := unstructured.NestedMap(h.upstream(clickHouseInstallationGVK, c.name()).Object, "spec", "configuration", "zookeeper"); found {
		t.Fatal("a single-replica installation points at a Keeper")
	}
	h.setStatus(clickHouseInstallationGVK, c.name(), map[string]interface{}{"status": "Completed"})
	h.poll()
	expectStatus(t, store.status(t, c.id), ServicePhaseReady, "service ready", c.endpoint)
}

func TestServicesKafkaReplicationFollowsBrokers(t *testing.T) {
	for instances, want := range map[int32][2]int64{1: {1, 1}, 2: {2, 1}, 3: {3, 2}} {
		resources := kafkaServiceKind{}.build(catalogService{name: "k", spec: lakeServiceSpec{Instances: instances, StorageGi: 1}}, "")
		config := resources[0].fields[1].value.(map[string]interface{})
		if config["offsets.topic.replication.factor"] != want[0] || config["default.replication.factor"] != want[0] ||
			config["min.insync.replicas"] != want[1] || config["transaction.state.log.min.isr"] != want[1] {
			t.Errorf("instances %d: config %v", instances, config)
		}
	}
}

func TestServicesUnknownKindFails(t *testing.T) {
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, "mongodb", `{}`, false),
		testService(testPostgresID, testPostgresSlug, "mysql", `{}`, false),
	}}
	store.services[1].Desired = serviceDesiredAbsent
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	expectStatus(t, store.status(t, testZettaServiceID), ServicePhaseFailed,
		`unknown service kind "mongodb": this operator runs zetta, postgres, kafka, clickhouse, tidb, cassandra, valkey, sqlite, duckdb, polars`, "")
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed,
		`unknown service kind "mysql": this operator runs zetta, postgres, kafka, clickhouse, tidb, cassandra, valkey, sqlite, duckdb, polars`, "")
}

func TestServicesCatalogNameLengthPerKind(t *testing.T) {
	// 21 characters: <lake>-<slug> is 38, room for Zetta's 15-character suffix but not for the
	// ClickHouse Keeper's chk-<name>-keeper-0-0 StatefulSet and its revision hash.
	lake := serviceTestLake()
	lake.Name = "production-west-eu-01"
	store := &fakeServiceStore{services: []LakeService{
		testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false),
		testService("77777777-7777-4777-8777-777777777777", "svc-777777777777", serviceKindClickHouse, `{}`, false),
		testService("99999999-9999-4999-8999-999999999999", "svc-999999999999", serviceKindCassandra, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	if phase := store.status(t, testZettaServiceID).Phase; phase != ServicePhaseProvisioning {
		t.Fatalf("zetta: %s", phase)
	}
	expectStatus(t, store.status(t, "77777777-7777-4777-8777-777777777777"), ServicePhaseFailed,
		`lake name "production-west-eu-01" is too long for service resource names`, "")
	// Cassandra's derived names come from the slug alone.
	if phase := store.status(t, "99999999-9999-4999-8999-999999999999").Phase; phase != ServicePhaseProvisioning {
		t.Fatalf("cassandra: %s", phase)
	}
}

func TestServicesCatalogCountsTowardQuota(t *testing.T) {
	lake := serviceTestLake()
	lake.Spec.Services.MaxServices = 1
	c := catalogCases()[4]
	store := &fakeServiceStore{services: []LakeService{
		testService(c.id, c.slug, c.kind, `{}`, false),
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
	}}
	h := newServiceHarness(t, lake, store)
	h.poll()
	h.poll()
	if phase := store.status(t, c.id).Phase; phase != ServicePhaseProvisioning {
		t.Fatalf("valkey: %s", phase)
	}
	expectStatus(t, store.status(t, testPostgresID), ServicePhaseFailed, "service quota reached (1)", "")

	// A new catalog service is refused over the quota before it writes anything.
	store2 := &fakeServiceStore{services: []LakeService{
		testService(testPostgresID, testPostgresSlug, serviceKindPostgres, `{}`, false),
		testService(c.id, c.slug, c.kind, `{}`, false),
	}}
	h2 := newServiceHarness(t, lake, store2)
	h2.poll()
	expectStatus(t, store2.status(t, c.id), ServicePhaseFailed, "service quota reached (1)", "")
	if h2.exists(valkeyClusterGVK, c.name()) {
		t.Fatal("a service over quota was created")
	}
	if err := h2.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: c.passwordSecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a service over quota generated a password: %v", err)
	}
}

// A Kafka that is not ready says what Strimzi is stuck on. A broker pod the API server refuses is
// recorded by Strimzi on the pod set, not on the Kafka; without reading it the service waits
// forever with no reason (operator#428).
func TestServicesKafkaSaysWhatStrimziIsStuckOn(t *testing.T) {
	in := catalogService{name: "prod-svc-083b8bddac01", namespace: "prod-ws-6bb777ba498a"}
	refusal := `Failure executing: POST at: https://10.0.0.1:443/api/v1/namespaces/prod-ws-6bb777ba498a/pods. Message: pods "prod-svc-083b8bddac01-pool-0" is forbidden: violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false`
	podSet := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "core.strimzi.io/v1", "kind": "StrimziPodSet",
		"metadata": map[string]interface{}{"name": in.name + "-pool", "namespace": in.namespace, "labels": map[string]interface{}{strimziClusterLabel: in.name}},
		"status":   map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Error", "status": "True", "message": refusal}}},
	}}
	other := podSet.DeepCopy()
	other.SetName("another-kafka-pool")
	other.SetLabels(map[string]string{strimziClusterLabel: "another-kafka"})
	unstructured.SetNestedField(other.Object, "not this one", "status", "conditions")
	kafka := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
			"type": "NotReady", "status": "True", "message": "Exceeded timeout of 300000ms while waiting for Pods resource"}}},
	}}
	found := map[string]*unstructured.Unstructured{strimziKafkaGVK.Kind: kafka}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(strimziPodSetListGVK, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "core.strimzi.io", Version: "v1", Kind: "StrimziPodSet"}, &unstructured.Unstructured{})

	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(podSet, other).Build()
	why, err := kafkaServiceKind{}.waitingReason(context.Background(), c, in, found)
	if err != nil || !strings.Contains(why, `violates PodSecurity "restricted:latest"`) {
		t.Fatalf("want the API server's refusal, got %q, %v", why, err)
	}

	// With no pod set failure, the Kafka's own NotReady message is the reason.
	c = fake.NewClientBuilder().WithScheme(scheme).Build()
	if why, err = (kafkaServiceKind{}).waitingReason(context.Background(), c, in, found); err != nil || !strings.HasPrefix(why, "Exceeded timeout of 300000ms") {
		t.Fatalf("want the Kafka's NotReady message, got %q, %v", why, err)
	}
	if got := boundedReason(strings.Repeat("x ", serviceWaitReasonLimit)); len(got) > serviceWaitReasonLimit+3 {
		t.Fatalf("an upstream message is bounded, got %d bytes", len(got))
	}
}

// The Kafka resources this operator writes carry the `restricted` security context themselves, on
// the broker pod and on the entity operator's pod, so the service does not depend on how Strimzi
// is configured on a cluster (operator#428).
func TestServicesKafkaPodsMeetRestrictedPodSecurity(t *testing.T) {
	resources := kafkaServiceKind{}.build(catalogService{name: "svc", namespace: "ws", spec: lakeServiceSpec{Instances: 1, StorageGi: 10}}, "")
	field := func(kind string, path ...string) interface{} {
		for _, resource := range resources {
			if resource.gvk.Kind != kind {
				continue
			}
			for _, f := range resource.fields {
				if strings.Join(f.path, ".") == strings.Join(path, ".") {
					return f.value
				}
			}
		}
		t.Fatalf("%s has no field %v", kind, path)
		return nil
	}
	restricted := func(what string, container map[string]interface{}) {
		t.Helper()
		sc, _ := container["securityContext"].(map[string]interface{})
		drop, _, _ := unstructured.NestedSlice(sc, "capabilities", "drop")
		seccomp, _, _ := unstructured.NestedString(sc, "seccompProfile", "type")
		if sc["allowPrivilegeEscalation"] != false || sc["runAsNonRoot"] != true || len(drop) != 1 || drop[0] != "ALL" || seccomp != "RuntimeDefault" {
			t.Errorf("%s does not meet restricted: %+v", what, sc)
		}
	}
	pool := field("KafkaNodePool", "spec", "template").(map[string]interface{})
	restricted("the broker container", pool["kafkaContainer"].(map[string]interface{}))
	if sc := pool["pod"].(map[string]interface{})["securityContext"].(map[string]interface{}); sc["runAsNonRoot"] != true {
		t.Errorf("the broker pod: %+v", sc)
	}
	entity := field("Kafka", "spec", "entityOperator").(map[string]interface{})["template"].(map[string]interface{})
	restricted("the topic operator container", entity["topicOperatorContainer"].(map[string]interface{}))
	restricted("the user operator container", entity["userOperatorContainer"].(map[string]interface{}))
}

// A one-broker Kafka costs a workspace one CPU of limits, like any other single-instance service.
// With no resources stated, its three containers each took the namespace default of one CPU, and
// the entity operator's pod was refused by a workspace quota of eight (operator#428).
func TestServicesKafkaCostsOneCPUOfLimits(t *testing.T) {
	limit := func(resources map[string]interface{}) resource.Quantity {
		t.Helper()
		cpu, _, _ := unstructured.NestedString(resources, "limits", "cpu")
		memory, _, _ := unstructured.NestedString(resources, "limits", "memory")
		request, _, _ := unstructured.NestedString(resources, "requests", "cpu")
		if cpu == "" || memory == "" || request == "" {
			t.Fatalf("resources must state CPU and memory limits and a CPU request, or the namespace default applies: %+v", resources)
		}
		return resource.MustParse(cpu)
	}
	total := limit(kafkaBrokerResources())
	for range 2 { // the topic operator and the user operator
		total.Add(limit(kafkaOperatorResources()))
	}
	if one := resource.MustParse("1"); total.Cmp(one) > 0 {
		t.Fatalf("a one-broker Kafka's containers have %s of CPU limits; more than 1 and it no longer costs what other services do", total.String())
	}

	resources := kafkaServiceKind{}.build(catalogService{name: "svc", namespace: "ws", spec: lakeServiceSpec{Instances: 1, StorageGi: 10}}, "")
	var poolHas, operatorsHave bool
	for _, r := range resources {
		for _, f := range r.fields {
			switch {
			case r.gvk.Kind == "KafkaNodePool" && strings.Join(f.path, ".") == "spec.resources":
				poolHas = true
			case r.gvk.Kind == "Kafka" && strings.Join(f.path, ".") == "spec.entityOperator":
				entity := f.value.(map[string]interface{})
				_, topic := entity["topicOperator"].(map[string]interface{})["resources"]
				_, user := entity["userOperator"].(map[string]interface{})["resources"]
				operatorsHave = topic && user
			}
		}
	}
	if !poolHas || !operatorsHave {
		t.Fatalf("resources are not on the rendered Kafka: broker %t, operators %t", poolHas, operatorsHave)
	}
}

// A pod of the entity operator that the API server refuses is the reason reported, ahead of the
// Kafka's own "timed out waiting".
func TestServicesKafkaReportsAQuotaRefusal(t *testing.T) {
	in := catalogService{name: "prod-svc-083b8bddac01", namespace: "prod-ws-6bb777ba498a"}
	refusal := `pods "prod-svc-083b8bddac01-entity-operator-7868887b77-vr562" is forbidden: exceeded quota: workspace-defaults, requested: limits.cpu=2, used: limits.cpu=8, limited: limits.cpu=8`
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: in.name + "-entity-operator", Namespace: in.namespace, Labels: map[string]string{strimziClusterLabel: in.name}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: refusal},
		}},
	}
	kafka := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
			"type": "NotReady", "status": "True", "message": "Exceeded timeout of 300000ms while waiting for Deployment resource"}}},
	}}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(strimziPodSetListGVK, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
	why, err := kafkaServiceKind{}.waitingReason(context.Background(), c, in, map[string]*unstructured.Unstructured{strimziKafkaGVK.Kind: kafka})
	if err != nil || why != refusal {
		t.Fatalf("want the quota refusal, got %q, %v", why, err)
	}
}

// Every kind that renders an upstream operator's resource carries the `restricted` security
// context a workspace namespace requires, and states its containers' sizes. On 2026-10-13 a Kafka
// and then a Cassandra were refused in a workspace because theirs carried neither (operator#428,
// operator#435). The check is on what is rendered: a field named securityContext must be
// restricted, and every kind must have at least one, and at least one stated resources block.
func TestServicesEveryUpstreamKindIsRestrictedAndSized(t *testing.T) {
	in := catalogService{name: "prod-svc-0123456789ab", namespace: "ws", svc: &LakeService{Slug: "svc-0123456789ab"},
		spec: lakeServiceSpec{Instances: 3, StorageGi: 10}}
	for name, kind := range map[string]catalogKind{
		"kafka": kafkaServiceKind{}, "cassandra": cassandraServiceKind{}, "clickhouse": clickHouseServiceKind{},
		"tidb": tidbServiceKind{}, "valkey": valkeyServiceKind{},
	} {
		t.Run(name, func(t *testing.T) {
			var containers, pods, sized int
			var walk func(path string, node interface{})
			walk = func(path string, node interface{}) {
				switch typed := node.(type) {
				case map[string]interface{}:
					for key, value := range typed {
						child, _ := value.(map[string]interface{})
						switch {
						case key == "securityContext" && child != nil && child["capabilities"] != nil:
							containers++
							drop, _, _ := unstructured.NestedSlice(child, "capabilities", "drop")
							seccomp, _, _ := unstructured.NestedString(child, "seccompProfile", "type")
							if child["allowPrivilegeEscalation"] != false || child["runAsNonRoot"] != true || len(drop) != 1 || drop[0] != "ALL" || seccomp != "RuntimeDefault" {
								t.Errorf("%s.%s is not restricted: %+v", path, key, child)
							}
						case (key == "securityContext" || key == "podSecurityContext") && child != nil:
							pods++
							seccomp, _, _ := unstructured.NestedString(child, "seccompProfile", "type")
							if child["runAsNonRoot"] != true || seccomp != "RuntimeDefault" {
								t.Errorf("%s.%s is not restricted: %+v", path, key, child)
							}
						case key == "limits" && child != nil && child["cpu"] != nil && child["memory"] != nil:
							sized++
						}
						walk(path+"."+key, value)
					}
				case []interface{}:
					for _, value := range typed {
						walk(path, value)
					}
				}
			}
			for _, resource := range kind.build(in, "password") {
				for _, field := range resource.fields {
					// A field whose own path ends in a security context or limits is one too.
					leaf := field.path[len(field.path)-1]
					walk(resource.gvk.Kind+"."+strings.Join(field.path[:len(field.path)-1], "."), map[string]interface{}{leaf: field.value})
				}
			}
			if containers == 0 || pods == 0 {
				t.Errorf("renders %d container and %d pod security contexts; a workspace namespace refuses a pod without them", containers, pods)
			}
			if sized == 0 {
				t.Error("states no CPU and memory limits; its containers would take the namespace default")
			}
		})
	}
}

// A service that is not ready reports Kubernetes' own last Warning about its objects, whatever
// its kind: the refusal of a pod appears only as an event on the workload that tried to create it.
func TestServicesReportTheLastWarningAboutTheirObjects(t *testing.T) {
	in := catalogService{name: "prod-svc-f341b215d67b", namespace: "prod-ws-1", svc: &LakeService{Slug: "svc-f341b215d67b"}}
	event := func(name, object, kind, message, when string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "Event",
			"metadata":       map[string]interface{}{"name": name, "namespace": in.namespace},
			"involvedObject": map[string]interface{}{"name": object},
			"type":           kind, "message": message, "lastTimestamp": when,
		}}
	}
	refusal := `Create Pod svc-f341b215d67b-svc-f341b215d67b-default-sts-0 failed error: pods "x" is forbidden: violates PodSecurity "restricted:latest"`
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "Event"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "EventList"}, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		event("old", "svc-f341b215d67b-svc-f341b215d67b-default-sts", "Warning", "an older warning", "2026-10-13T18:00:00Z"),
		event("new", "svc-f341b215d67b-svc-f341b215d67b-default-sts", "Warning", refusal, "2026-10-13T19:00:00Z"),
		event("noise", "svc-f341b215d67b", "Warning", "pod x: pod not found, most likely statefulset controller has not caught up yet", "2026-10-13T19:30:00Z"),
		event("normal", "svc-f341b215d67b-svc-f341b215d67b-default-sts", "Normal", "created", "2026-10-13T19:40:00Z"),
		event("other", "prod-svc-000000000000-0", "Warning", "another service's problem", "2026-10-13T19:50:00Z"),
	).Build()
	why, err := serviceWarningReason(context.Background(), c, in)
	if err != nil || why != refusal {
		t.Fatalf("want the newest real warning about this service's objects, got %q, %v", why, err)
	}
}

// The workspace quota does not cap the sum of CPU limits: it reported a workspace using a third
// of a CPU as full, and nothing new could start in it (operator#435).
func TestTheDefaultWorkspacePlanDoesNotCapTheSumOfCPULimits(t *testing.T) {
	plan := defaultWorkspacePlan()
	if _, capped := plan.Quota[corev1.ResourceLimitsCPU]; capped {
		t.Fatal("the default plan caps limits.cpu again")
	}
	// A workspace must hold one service of every kind in the catalog at once. Counted on
	// lake.oramadev.net: Postgres 3 Service objects, Cassandra 5, Kafka 2 or 3, the rest 1 to 5.
	services := plan.Quota[corev1.ResourceServices]
	if services.Value() < 60 {
		t.Errorf("the default plan allows %d Service objects; ten kinds of datastore need more", services.Value())
	}
	for _, kept := range []corev1.ResourceName{corev1.ResourceRequestsCPU, corev1.ResourceRequestsMemory, corev1.ResourceLimitsMemory, corev1.ResourcePods} {
		if _, ok := plan.Quota[kept]; !ok {
			t.Errorf("the default plan no longer caps %s", kept)
		}
	}
}
