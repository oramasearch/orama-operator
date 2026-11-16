// SPDX-License-Identifier: MIT

package controller

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestDeploymentCredentialsRejectInstanceAdminOnEverySurface(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(deploymentControlPlaneScheme(t)).
		WithObjects(deploymentControlPlaneObjects(false)...).Build()
	double := newContractSQLDouble(t)
	server := &DeploymentControlPlane{Client: c, ZettaURL: double.url()}
	for _, endpoint := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/create_deployment", "{}"},
		{http.MethodPost, "/v1/rollback_deployment", "{}"},
		{http.MethodGet, "/v1/list_deployments", ""},
		{http.MethodGet, "/v1/deployment_status", ""},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_deployments","arguments":{}}}`},
	} {
		t.Run(endpoint.path+endpoint.body, func(t *testing.T) {
			response := controlPlaneRequest(t, server.Handler(), endpoint.method, endpoint.path, endpoint.body, "instance-admin-token")
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("instance admin accepted: HTTP %d", response.Code)
			}
		})
	}
	if target, err := server.authenticate(ctx, "Bearer environment-token"); err != nil || target.Lake.Name != "prod" {
		t.Fatal("deploy token did not select its Lake")
	}
	// A missing or empty deployment Secret must never fall back to the admin credential.
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: "lake", Name: lakeDeploySecret("prod")}
	if err := c.Get(ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	secret.Data = nil
	if err := c.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"empty", "missing"} {
		if state == "missing" {
			if err := c.Delete(ctx, &secret); err != nil {
				t.Fatal(err)
			}
		}
		for _, token := range []string{"instance-admin-token", "environment-token"} {
			if _, err := server.authenticate(ctx, "Bearer "+token); err == nil || err.Status != http.StatusUnauthorized {
				t.Errorf("%s deploy Secret: authentication did not fail closed", state)
			}
		}
	}
}

func TestLakeDeploymentCredentialIsIndependentAndNeverInjected(t *testing.T) {
	ctx := context.Background()
	scheme := deploymentControlPlaneScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: "lake-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).Build()
	r := &LakeReconciler{Client: c, Scheme: scheme}
	if err := r.reconcileLakeAdminToken(ctx, lake, "lake", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileLakeDeployToken(ctx, lake, "lake", nil); err != nil {
		t.Fatal(err)
	}
	var admin, deploy corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "lake", Name: lakeAdminSecret(lake.Name)}, &admin); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "lake", Name: lakeDeploySecret(lake.Name)}
	if err := c.Get(ctx, key, &deploy); err != nil {
		t.Fatal(err)
	}
	if len(deploy.Data["token"]) != len("lake_deploy_")+64 || !strings.HasPrefix(string(deploy.Data["token"]), "lake_deploy_") ||
		bytes.Equal(admin.Data["token"], deploy.Data["token"]) || !metav1.IsControlledBy(&deploy, lake) {
		t.Fatal("deployment token is not an independent, Lake-owned credential")
	}
	if err := r.reconcileLakeDeployToken(ctx, lake, "lake", nil); err != nil {
		t.Fatal(err)
	}
	var again corev1.Secret
	if err := c.Get(ctx, key, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(deploy.Data["token"], again.Data["token"]) || deploy.ResourceVersion != again.ResourceVersion {
		t.Fatal("steady reconciliation changed the deployment credential")
	}
	var adminRef string
	for _, env := range lakeEnv(lake, lakeSealingMaterial{}) {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := env.ValueFrom.SecretKeyRef.Name
		if ref == deploy.Name {
			t.Fatal("Lake received the deployment credential")
		}
		if env.Name == "LAKE_ADMIN_TOKEN" {
			adminRef = ref
		}
	}
	if adminRef != admin.Name {
		t.Fatal("Lake admin authentication is no longer wired to the independent admin Secret")
	}
}
