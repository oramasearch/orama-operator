package controller

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestLakeControlDBRolesAndGrantsGate(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "roles", UID: "roles-proof"}, Spec: oramav1alpha1.LakeSpec{Image: "oramasearch/lake:old"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(lake, &oramav1alpha1.PostgresSchema{}).Build()
	reconcileLake(t, c, scheme, lake.Name)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "lake-dev", Name: lakeControlDB(lake.Name)}, cluster); err != nil {
		t.Fatal(err)
	}
	roles, _, err := unstructured.NestedSlice(cluster.Object, "spec", "managed", "roles")
	if err != nil || !reflect.DeepEqual(roles, lakeManagedRoles(lake)) {
		t.Fatal("CNPG must provision both restricted runtime roles")
	}
	for _, raw := range roles {
		role := raw.(map[string]interface{})
		for _, attribute := range []string{"superuser", "createdb", "createrole", "replication", "bypassrls", "inherit"} {
			if role[attribute] != false {
				t.Fatalf("CNPG role must disable %s", attribute)
			}
		}
		if role["connectionLimit"] != int64(40) {
			t.Fatal("runtime connections must be bounded")
		}
	}
	versions := map[string]string{}
	for _, role := range []string{lakeRuntimeRole, lakeOperatorRole} {
		var secret corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: "lake-dev", Name: lakeRoleSecret(lake.Name, role)}, &secret); err != nil {
			t.Fatal(err)
		}
		if string(secret.Data["username"]) != role || len(secret.Data["password"]) != 48 || secret.Labels["cnpg.io/reload"] != "true" || !metav1.IsControlledBy(&secret, lake) {
			t.Fatal("invalid role credential")
		}
		if errors := validation.IsDNS1123Subdomain(secret.Name); len(errors) != 0 {
			t.Fatal(errors)
		}
		versions[role] = secret.ResourceVersion
	}
	grantsKey := client.ObjectKey{Namespace: "lake-dev", Name: lakeControlDB(lake.Name) + "-grants"}
	if err := c.Get(ctx, grantsKey, &oramav1alpha1.PostgresSchema{}); !apierrors.IsNotFound(err) {
		t.Fatal("grants must wait for the pinned schema")
	}
	setLakeSchemaAppliedForTest(t, c, client.ObjectKey{Namespace: "lake-dev", Name: lakeControlDB(lake.Name) + "-schema"})
	reconcileLake(t, c, scheme, lake.Name)
	var grants oramav1alpha1.PostgresSchema
	if err := c.Get(ctx, grantsKey, &grants); err != nil {
		t.Fatal(err)
	}
	if !grants.Spec.ApplyEveryVersion || grants.Spec.ConnectionSecretRef.Name != lakeControlDB(lake.Name)+"-app" {
		t.Fatal("grants must run as the migration owner for every policy version")
	}
	deploymentKey := client.ObjectKey{Namespace: "lake-dev", Name: "lake-" + lake.Name}
	if err := c.Get(ctx, deploymentKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatal("schema alone must not release runtime rollout")
	}
	applyLakeSchemaForWorkloadTest(t, c, scheme, lake)
	reconcileLake(t, c, scheme, lake.Name)
	var deployment appsv1.Deployment
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LAKE_CONTROL_DB_URI", "LAKE_CONTROL_DB_PASSWORD"} {
		env := envValue(t, deployment.Spec.Template.Spec.Containers[0].Env, key)
		if ref := env.ValueFrom.SecretKeyRef; ref.Name != lakeRoleSecret(lake.Name, lakeRuntimeRole) || ref.Optional != nil && *ref.Optional {
			t.Fatal("Lake must require the runtime credential")
		}
	}
	assertLakeSchemaBlocksUpdate(t, c, scheme, lake, grantsKey, deploymentKey)
	for _, role := range []string{lakeRuntimeRole, lakeOperatorRole} {
		var secret corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: "lake-dev", Name: lakeRoleSecret(lake.Name, role)}, &secret); err != nil {
			t.Fatal(err)
		}
		if secret.ResourceVersion != versions[role] {
			t.Fatal("steady reconciliation must preserve credentials and avoid writes")
		}
	}
	// A lost Secret is recreated and CNPG adopts its new password. Existing env-based clients
	// must roll once to consume it, then converge without further Deployment writes.
	applyLakeSchemaForWorkloadTest(t, c, scheme, lake)
	reconcileLake(t, c, scheme, lake.Name)
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	previousHash := deployment.Spec.Template.Annotations[lakeRuntimeCredentialHashKey]
	if previousHash == "" {
		t.Fatal("missing runtime credential digest")
	}
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "lake-dev", Name: lakeRoleSecret(lake.Name, lakeRuntimeRole)}}); err != nil {
		t.Fatal(err)
	}
	reconcileLake(t, c, scheme, lake.Name)
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[lakeRuntimeCredentialHashKey] == previousHash {
		t.Fatal("credential recovery did not roll Lake's stale environment")
	}
	version := deployment.ResourceVersion
	reconcileLake(t, c, scheme, lake.Name)
	if err := c.Get(ctx, deploymentKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ResourceVersion != version {
		t.Fatal("recovered credentials cause repeated rollouts")
	}
}
