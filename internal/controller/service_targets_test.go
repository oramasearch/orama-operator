/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The shapes below are copied from Lake/prod's workspace prod-ws-6bb777ba498a on 2026-10-14, where
// every app-to-service Connection waited forever for "the target's Kubernetes Service ports":
// neither the upstream operators' pods nor their Services carry orama.cloud/service-id.
func serviceTargetFixture(t *testing.T) client.Client {
	t.Helper()
	const ns = "prod-ws-6bb777ba498a"
	upstream := func(gvkGroup, version, kind, name, id, serviceKind string) *unstructured.Unstructured {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion(gvkGroup + "/" + version)
		object.SetKind(kind)
		object.SetNamespace(ns)
		object.SetName(name)
		object.SetLabels(map[string]string{serviceIDLabel: id, serviceKindLabel: serviceKind})
		return object
	}
	service := func(name string, selector map[string]string, ports ...int32) *corev1.Service {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Selector: selector}}
		for _, port := range ports {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Port: port, Protocol: corev1.ProtocolTCP})
		}
		return s
	}
	headless := func(s *corev1.Service) *corev1.Service {
		s.Spec.ClusterIP = corev1.ClusterIPNone
		return s
	}
	zetta := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ns,
		Labels: map[string]string{serviceIDLabel: "zetta-id"}}}
	tidbSelector := func(component string) map[string]string {
		return map[string]string{"app.kubernetes.io/component": component, "app.kubernetes.io/instance": "prod-svc-4f18cc589625",
			"app.kubernetes.io/managed-by": "tidb-operator", "app.kubernetes.io/name": "tidb-cluster"}
	}
	zettaSelector := map[string]string{"app.kubernetes.io/component": "zetta-node", "app.kubernetes.io/instance": ns,
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", "app.kubernetes.io/name": "orama-zetta"}
	scheme := newLakeScheme(t)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		zetta,
		upstream("postgresql.cnpg.io", "v1", "Cluster", "prod-svc-5103dc69b44c", "postgres-id", serviceKindPostgres),
		upstream("pingcap.com", "v1alpha1", "TidbCluster", "prod-svc-4f18cc589625", "tidb-id", serviceKindTiDB),
		service("prod-svc-5103dc69b44c-rw", map[string]string{cnpgClusterLabel: "prod-svc-5103dc69b44c", "cnpg.io/instanceRole": "primary"}, 5432),
		service("prod-svc-5103dc69b44c-ro", map[string]string{cnpgClusterLabel: "prod-svc-5103dc69b44c", "cnpg.io/instanceRole": "replica"}, 5432),
		service("prod-svc-4f18cc589625-tidb", tidbSelector("tidb"), 4000, 10080),
		service("prod-svc-4f18cc589625-pd", tidbSelector("pd"), 2379),
		service(ns+"-zetta", zettaSelector, 4800),
		headless(service(ns+"-zetta-headless", zettaSelector, 7777, 4800)),
		service("app-4578260eadb0", map[string]string{"orama.cloud/app": "other"}, 80),
	).Build()
}

func TestResolveServiceTargetFromItsPrimaryResource(t *testing.T) {
	ctx := context.Background()
	c := serviceTargetFixture(t)
	const ns = "prod-ws-6bb777ba498a"
	cases := []struct {
		id, kind string
		pods     map[string]string
		services []string
	}{
		{"postgres-id", serviceKindPostgres, map[string]string{cnpgClusterLabel: "prod-svc-5103dc69b44c"},
			[]string{"prod-svc-5103dc69b44c-ro", "prod-svc-5103dc69b44c-rw"}},
		{"tidb-id", serviceKindTiDB, map[string]string{"app.kubernetes.io/component": "tidb",
			"app.kubernetes.io/instance": "prod-svc-4f18cc589625", "app.kubernetes.io/managed-by": "tidb-operator"},
			[]string{"prod-svc-4f18cc589625-tidb"}},
		{"zetta-id", serviceKindZetta, map[string]string{"app.kubernetes.io/component": "zetta-node", "app.kubernetes.io/instance": ns},
			[]string{ns + "-zetta"}},
	}
	for _, tc := range cases {
		target, err := resolveServiceTarget(ctx, c, "prod", ns, tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if target.Kind != tc.kind || !reflect.DeepEqual(target.Pods, tc.pods) {
			t.Errorf("%s: got kind %q pods %v, want %q %v", tc.id, target.Kind, target.Pods, tc.kind, tc.pods)
		}
		selector := metav1.LabelSelector{MatchLabels: target.Pods}
		labelSelector, _ := metav1.LabelSelectorAsSelector(&selector)
		services, err := targetServices(ctx, c, ns, selector, labelSelector)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, service := range services {
			names = append(names, service.Name)
		}
		if !reflect.DeepEqual(names, tc.services) {
			t.Errorf("%s: Services %v, want %v (never PD, never another service's)", tc.id, names, tc.services)
		}
	}
	// With no primary resource, pods that carry the id themselves are the target.
	fallback, err := resolveServiceTarget(ctx, c, "prod", ns, "unlabelled-id")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fallback.Pods, map[string]string{serviceIDLabel: "unlabelled-id"}) || fallback.Kind != "" {
		t.Errorf("fallback = %+v", fallback)
	}
}

func TestEveryCatalogKindHasAPodSelector(t *testing.T) {
	for name, kind := range catalogKinds {
		in := catalogService{name: "prod-x", svc: &LakeService{ID: "id", Kind: name, Slug: "x"}}
		if len(kind.podSelector(in)) == 0 {
			t.Errorf("%s has an empty podSelector; a Connection to it would select every pod", name)
		}
	}
}
