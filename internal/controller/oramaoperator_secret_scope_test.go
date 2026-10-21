/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// orama-operator#832: OramaOperator copies its pull Secret from the managing cluster to the cluster
// it installs onto, so it keeps the same boundary as a set (managedset_secret_scope.go): a Secret
// production uses is never copied to a non-production cluster. What is production is read from the
// resources only: each cluster's purpose tag, the sets and OramaOperators that name a Secret, and
// the Secret's own orama.cloud/purpose label.

const (
	scopeNonprodCluster = "orama-nonprod-cluster-01"
	scopeProdCluster    = "orama-production-cluster-02"
)

// operatorOn is an OramaOperator for cluster that copies orama-system/<pullSecret>.
func operatorOn(cluster, pullSecret string) *oramav1alpha1.OramaOperator {
	operator := testOramaOperator("v0.7.3")
	operator.Name = cluster
	operator.Spec.ClusterRef = cluster
	operator.Spec.ImagePullSecretRef = oramav1alpha1.ClusterSecretReference{Name: pullSecret, Namespace: "orama-system"}
	return operator
}

// pullSecretNamed is a registry credentials Secret in orama-system, with labels.
func pullSecretNamed(name string, labels map[string]string) *corev1.Secret {
	secret := dockerHubSecret()
	secret.Name, secret.Labels = name, labels
	return secret
}

// clustersForScope is the nonprod cluster and a production cluster, both Ready.
func clustersForScope() []client.Object {
	return append(managedCluster(scopeNonprodCluster, "westus2", "nonprod"),
		managedCluster(scopeProdCluster, "swedencentral", "production")...)
}

func reconcileOperatorNamed(t *testing.T, r *OramaOperatorReconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func operatorNamed(t *testing.T, c client.Client, name string) *oramav1alpha1.OramaOperator {
	t.Helper()
	var operator oramav1alpha1.OramaOperator
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &operator); err != nil {
		t.Fatal(err)
	}
	return &operator
}

// The defect as committed: the nonprod and production OramaOperators both named
// orama-system/oramasearch-dockerhub, so production's Docker Hub token was copied to the nonprod
// cluster. The nonprod one is refused and nothing reaches its cluster; production is unchanged.
func TestAProductionOperatorsPullSecretIsNeverCopiedToANonprodCluster(t *testing.T) {
	objects := append(clustersForScope(),
		operatorOn(scopeNonprodCluster, "oramasearch-dockerhub"),
		operatorOn(scopeProdCluster, "oramasearch-dockerhub"),
		pullSecretNamed("oramasearch-dockerhub", nil))
	c := leafTestClient(t, objects...)
	leaf := &fakeLeaf{}
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileOperatorNamed(t, r, scopeNonprodCluster); err != nil {
		t.Fatalf("a refusal is the resource as written, recorded in its status, not an error to retry: %v", err)
	}
	if len(leaf.operatorRequests) != 0 {
		t.Fatalf("the nonprod cluster was reached with pull Secret %s", leaf.operatorRequests[0].PullSecret.Name)
	}
	want := "Secret orama-system/oramasearch-dockerhub is the pull Secret of production cluster " + scopeProdCluster +
		" (OramaOperator " + scopeProdCluster + "), so it is a production credential and is never copied to a nonprod cluster"
	got := operatorNamed(t, c, scopeNonprodCluster)
	if got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, want) {
		t.Fatalf("status: phase %q, message %q\nwant the message to contain %q", got.Status.Phase, got.Status.Message, want)
	}
	if !strings.Contains(got.Status.Message, "give this cluster its own pull Secret") {
		t.Fatalf("the refusal does not say what to do: %q", got.Status.Message)
	}

	if _, err := reconcileOperatorNamed(t, r, scopeProdCluster); err != nil {
		t.Fatalf("the production operator is unchanged: %v", err)
	}
	if len(leaf.operatorRequests) != 1 || leaf.operatorRequests[0].PullSecret.Name != "oramasearch-dockerhub" {
		t.Fatalf("production cluster: %+v", leaf.operatorRequests)
	}
}

// A Secret a production set copies (v1-prod copies orama-system/oramasearch-dockerhub into v1-eu)
// is a production credential too, whichever OramaOperator names it.
func TestAProductionSetsSecretIsNeverAClusterOperatorsPullSecret(t *testing.T) {
	objects := append(v1Sets([]string{"oramasearch-dockerhub"}, nil),
		operatorOn(scopeNonprodCluster, "oramasearch-dockerhub"),
		pullSecretNamed("oramasearch-dockerhub", nil))
	c := leafTestClient(t, objects...)
	leaf := &fakeLeaf{}
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileOperatorNamed(t, r, scopeNonprodCluster); err != nil {
		t.Fatal(err)
	}
	want := "Secret orama-system/oramasearch-dockerhub is copied into production members by OramaCloudV1Set v1-prod"
	if got := operatorNamed(t, c, scopeNonprodCluster); got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, want) {
		t.Fatalf("status: phase %q, message %q", got.Status.Phase, got.Status.Message)
	}
	if len(leaf.operatorRequests) != 0 {
		t.Fatal("the nonprod cluster was reached")
	}
}

// The reverse direction, for the sets: a Secret a production OramaOperator copies is never copied
// into a nonprod set member either.
func TestAProductionOperatorsPullSecretIsNeverCopiedIntoANonprodMember(t *testing.T) {
	objects := append(v1Sets(nil, []string{"oramasearch-dockerhub"}),
		operatorOn(scopeProdCluster, "oramasearch-dockerhub"),
		orgSecret("oramasearch-dockerhub", nil))
	c := setTestClient(t, objects...)
	members := &fakeMembers{ready: map[string]bool{"v1-dev": true}}
	err := reconcileV1SetNamed(t, c, members, "v1-nonprod")
	want := "Secret orama-system/oramasearch-dockerhub is the pull Secret of production cluster " + scopeProdCluster
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want the production operator's pull Secret refused, got %v", err)
	}
	if len(members.applied) != 0 {
		t.Fatal("the nonprod member was applied")
	}
}

// The fix in config/clusters: the nonprod cluster's OramaOperator names its own Secret,
// orama-system/oramasearch-dockerhub-nonprod, copied as oramasearch-dockerhub
// (spec.imagePullSecretAs), so the copy of production's token already on the cluster is overwritten
// in place and the cluster's operator keeps the pull secret name it has.
func TestANonprodOperatorsOwnPullSecretIsCopiedUnderTheNameItReplaces(t *testing.T) {
	nonprod := operatorOn(scopeNonprodCluster, "oramasearch-dockerhub-nonprod")
	nonprod.Spec.ImagePullSecretAs = "oramasearch-dockerhub"
	own := pullSecretNamed("oramasearch-dockerhub-nonprod", nil)
	own.Data = map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://index.docker.io/v1/":{"username":"nonprod","password":"nonprod-token"}}}`)}
	objects := append(clustersForScope(), nonprod, operatorOn(scopeProdCluster, "oramasearch-dockerhub"),
		pullSecretNamed("oramasearch-dockerhub", nil), own)
	c := leafTestClient(t, objects...)
	leaf := &fakeLeaf{}
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileOperatorNamed(t, r, scopeNonprodCluster); err != nil {
		t.Fatal(err)
	}
	if len(leaf.operatorRequests) != 1 {
		t.Fatalf("not installed: %+v", operatorNamed(t, c, scopeNonprodCluster).Status)
	}
	req := leaf.operatorRequests[0]
	if req.PullSecret.Name != "oramasearch-dockerhub" || !strings.Contains(string(req.PullSecret.Data[corev1.DockerConfigJsonKey]), "nonprod-token") {
		t.Fatalf("copied %s with %s", req.PullSecret.Name, req.PullSecret.Data[corev1.DockerConfigJsonKey])
	}
	if req.Chart.User != "nonprod" || req.Chart.Pass != "nonprod-token" {
		t.Fatalf("the chart is pulled with %q, not the nonprod credential", req.Chart.User)
	}
	pullSecrets := req.Values["operator"].(map[string]any)["imagePullSecrets"].([]any)
	if pullSecrets[0].(map[string]any)["name"] != "oramasearch-dockerhub" {
		t.Fatalf("the cluster's operator pulls with %v", pullSecrets)
	}
	// The source on the managing cluster is read, never written.
	var source corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "orama-system", Name: "oramasearch-dockerhub-nonprod"}, &source); err != nil || source.Name != "oramasearch-dockerhub-nonprod" {
		t.Fatalf("source: %v", err)
	}
}

// A Secret labelled with a purpose reaches only clusters of that purpose, in either direction.
func TestALabelledPullSecretReachesOnlyClustersOfItsPurpose(t *testing.T) {
	for name, tc := range map[string]struct{ cluster, label, want string }{
		"production into nonprod": {scopeNonprodCluster, "production", "Secret orama-system/hub is labelled orama.cloud/purpose=production and the target cluster is nonprod: it is never copied there"},
		"nonprod into production": {scopeProdCluster, "nonprod", "Secret orama-system/hub is labelled orama.cloud/purpose=nonprod and the target cluster is production: it is never copied there"},
		"nonprod into nonprod":    {scopeNonprodCluster, "nonprod", ""},
	} {
		t.Run(name, func(t *testing.T) {
			objects := append(clustersForScope(), operatorOn(tc.cluster, "hub"), pullSecretNamed("hub", map[string]string{secretPurposeLabel: tc.label}))
			c := leafTestClient(t, objects...)
			leaf := &fakeLeaf{}
			r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}
			if _, err := reconcileOperatorNamed(t, r, tc.cluster); err != nil {
				t.Fatal(err)
			}
			got := operatorNamed(t, c, tc.cluster)
			if tc.want == "" {
				if len(leaf.operatorRequests) != 1 {
					t.Fatalf("refused: %q", got.Status.Message)
				}
				return
			}
			if len(leaf.operatorRequests) != 0 || got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, tc.want) {
				t.Fatalf("requests %d, phase %q, message %q", len(leaf.operatorRequests), got.Status.Phase, got.Status.Message)
			}
		})
	}
}

// A cluster with no purpose tag is not production: production's Secrets do not reach it either.
func TestAClusterWithoutAPurposeGetsNoProductionPullSecret(t *testing.T) {
	objects := append(managedCluster("untagged", "westus2", ""), operatorOn("untagged", "oramasearch-dockerhub"))
	objects = append(objects, clustersForScope()...)
	objects = append(objects, operatorOn(scopeProdCluster, "oramasearch-dockerhub"), pullSecretNamed("oramasearch-dockerhub", nil))
	c := leafTestClient(t, objects...)
	leaf := &fakeLeaf{}
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}
	if _, err := reconcileOperatorNamed(t, r, "untagged"); err != nil {
		t.Fatal(err)
	}
	if got := operatorNamed(t, c, "untagged"); len(leaf.operatorRequests) != 0 || !strings.Contains(got.Status.Message, "never copied to a cluster with no purpose tag") {
		t.Fatalf("requests %d, message %q", len(leaf.operatorRequests), got.Status.Message)
	}
}
