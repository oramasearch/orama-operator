/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The contract (operator#560: lake's format is the contract and the operator conforms). These are
// lake's own statements, copied from oramasearch/lake at origin/dev 34718173:
//   - the SELECTs are PlatformService::sync (src/control/platform.cpp): "SELECT " + the column
//     list of include/lake/control/zetta_control.hpp + " FROM " + table + " WHERE lake = " +
//     quote_zetta_literal(LAKE_PUBLIC_HOST), run against database orama_control;
//   - the INSERTs are ZettaControlClient::append (src/control/zetta_control.cpp) of the rows
//     PlatformService::set_environment_env and ::change_domain build: every column in that
//     order, strings quoted, `deleted` a SQL boolean.
//
// If lake changes a column, these strings change with it, and this test says what the operator
// must write.
const (
	lakeSelectDeployments    = "SELECT operator, lake, repository, environment, kind, change, image, deployment_id, requested_by, requested_at, phase, namespace, url, message, updated_at, ordering_token FROM deployments WHERE lake = 'lake-dev.oramadev.net'"
	lakeSelectEnvironmentEnv = "SELECT lake, repository, environment, key, value, deleted, set_by, updated_at, ordering_token FROM environment_env WHERE lake = 'lake-dev.oramadev.net'"
	lakeSelectDomains        = "SELECT lake, repository, environment, host, tls, status, message, deleted, requested_by, updated_at, ordering_token FROM domains WHERE lake = 'lake-dev.oramadev.net'"

	lakeSetGreeting   = "INSERT INTO environment_env (lake, repository, environment, key, value, deleted, set_by, updated_at, ordering_token) VALUES ('lake-dev.oramadev.net', '0f8fad5b-d9cb-469f-a165-70867728950e', 'production', 'GREETING', 'hello', FALSE, 'user/u-1', '2026-10-17T10:00:01.000000Z', '7c9e6679-7425-40de-944b-e07fc1f90ae7')"
	lakeUnsetGreeting = "INSERT INTO environment_env (lake, repository, environment, key, value, deleted, set_by, updated_at, ordering_token) VALUES ('lake-dev.oramadev.net', '0f8fad5b-d9cb-469f-a165-70867728950e', 'production', 'GREETING', '', TRUE, 'user/u-1', '2026-10-17T10:05:00.000000Z', '9a7b330a-a736-41e5-8b0c-1c6c2d6a2f10')"
	lakeCreateDomain  = "INSERT INTO domains (lake, repository, environment, host, tls, status, message, deleted, requested_by, updated_at, ordering_token) VALUES ('lake-dev.oramadev.net', '0f8fad5b-d9cb-469f-a165-70867728950e', 'production', 'shop.example.com', 'acme', 'pending', '', FALSE, 'user/u-1', '2026-10-17T10:00:02.000000Z', 'e4eaaaf2-d142-11e1-b3e4-080027620cdd')"
	lakeDeleteDomain  = "INSERT INTO domains (lake, repository, environment, host, tls, status, message, deleted, requested_by, updated_at, ordering_token) VALUES ('lake-dev.oramadev.net', '0f8fad5b-d9cb-469f-a165-70867728950e', 'production', 'shop.example.com', 'acme', 'pending', '', TRUE, 'user/u-1', '2026-10-17T10:05:01.000000Z', '16fd2706-8baf-433b-82eb-8c7fada847da')"

	lakeContractRepository = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

// lakeTimePattern is lake's row time: UTC with exactly six fractional digits.
var lakeTimePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)

// lakeLatestByKey is lake's latest_by_key (src/control/platform.cpp): it refuses the whole sync
// on a row without its key, repository, environment or updated_at ("incomplete platform row",
// which degrades lake's platform), and keeps the greatest row per key by platform_row_order
// (updated_at widened to six digits, then ordering_token, then the row).
func lakeLatestByKey(t *testing.T, rows []map[string]any, key func(map[string]any) string) map[string]map[string]any {
	t.Helper()
	text := func(row map[string]any, column string) string {
		value, found := row[column]
		if !found || value == nil {
			return ""
		}
		if s, ok := value.(string); ok {
			return s
		}
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	order := func(row map[string]any) string {
		updatedAt := text(row, "updated_at")
		if strings.HasSuffix(updatedAt, "Z") && !strings.Contains(updatedAt, ".") {
			updatedAt = strings.TrimSuffix(updatedAt, "Z") + ".000000Z"
		}
		encoded, _ := json.Marshal(row)
		return updatedAt + "\x00" + text(row, "ordering_token") + "\x00" + string(encoded)
	}
	latest := map[string]map[string]any{}
	for _, row := range rows {
		if key(row) == "" || text(row, "repository") == "" || text(row, "environment") == "" || text(row, "updated_at") == "" {
			t.Fatalf("lake would refuse this sync: incomplete platform row %+v", row)
		}
		current, found := latest[key(row)]
		if !found || order(row) > order(current) {
			latest[key(row)] = row
		}
	}
	return latest
}

func byDeploymentID(row map[string]any) string { s, _ := row["deployment_id"].(string); return s }
func byDomain(row map[string]any) string {
	repository, _ := row["repository"].(string)
	environment, _ := row["environment"].(string)
	host, _ := row["host"].(string)
	if repository == "" || host == "" {
		return ""
	}
	return repository + "/" + environment + "/" + host
}

// lakeReadsAll runs lake's three SELECTs, as one sync does: all three must succeed.
func lakeReadsAll(t *testing.T, zetta *fakeZetta) (deployments, env, domains []map[string]any) {
	t.Helper()
	return zetta.mustExec(t, lakeSelectDeployments), zetta.mustExec(t, lakeSelectEnvironmentEnv), zetta.mustExec(t, lakeSelectDomains)
}

// The rows the operator wrote before operator#560 fail lake's SELECT, which is the bug: the fake
// enforces what Zetta enforces.
func TestLakeRefusesTheOperatorsOldRows(t *testing.T) {
	zetta := newFakeZetta(t, "test-token")
	zetta.mustExec(t, "INSERT INTO deployments (deployment_id, record_type, lake, repository, environment, kind, change_id, image, updated_at) VALUES ('d', 'desired', 'lake-dev.oramadev.net', 'r', 'production', 'named', 'c', 'i', '2026-10-17T10:00:00Z')")
	if _, err := zetta.exec("orama_control", lakeSelectDeployments); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("lake's SELECT over the old rows = %v, want an unknown-column error", err)
	}
}

// A deployment created through the operator's /v1 contract, then lake's environment value and
// custom domain, become one LakeEnvironment; lake's own SELECTs succeed after every operator write,
// lake sees the operator's status rows, and removing the value and the domain removes them.
func TestLakeSeesTheOperatorsDeploymentEnvAndDomainRows(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{
		IngressEnabled: true,
		DNSSecretRef:   &corev1.LocalObjectReference{Name: "cloudflare"},
	})
	objects := []client.Object{
		lake,
		&oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-zetta", Namespace: "lake"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeAdminSecret("prod"), Namespace: "lake"}, Data: map[string][]byte{"token": []byte("environment-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-zetta-zetta-admin", Namespace: "lake"}, Data: map[string][]byte{"token": []byte("test-token")}},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.LakeEnvironment{}, &appsv1.Deployment{}, &networkingv1.Ingress{}, &oramav1alpha1.CloudflareDNSRecord{}).
		Build()
	zetta := newFakeZetta(t, "test-token")
	lakeReadsAll(t, zetta) // an empty control plane

	// 1. liquid deploys through the operator's contract: the desired row is in lake's shape.
	plane := &DeploymentControlPlane{Client: kubeClient, ZettaURL: zetta.url(), Now: func() time.Time {
		return time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	}}
	create := fmtJSON(map[string]any{
		"repository":  lakeContractRepository,
		"environment": "production",
		"change":      "blake3:" + strings.Repeat("1", 64),
		"image":       testEnvironmentImage,
	})
	record := decodeDeploymentResponse(t, controlPlaneRequest(t, plane.Handler(), http.MethodPost, "/v1/create_deployment", create, "environment-token"))
	deployments, _, _ := lakeReadsAll(t, zetta)
	if len(deployments) != 1 {
		t.Fatalf("lake reads %d deployment rows, want the desired row", len(deployments))
	}
	desired := deployments[0]
	for column, want := range map[string]string{
		"lake": testLakeHost, "repository": lakeContractRepository, "environment": "production",
		"kind": "named", "change": "blake3:" + strings.Repeat("1", 64), "image": testEnvironmentImage,
		"deployment_id": record.ID, "requested_by": "lake-admin-token/prod", "phase": deploymentPhaseRequested,
	} {
		if desired[column] != want {
			t.Errorf("desired row %s = %v, want %q", column, desired[column], want)
		}
	}
	for _, column := range []string{"operator", "ordering_token"} {
		if desired[column] == "" {
			t.Errorf("desired row has no %s", column)
		}
	}
	for _, column := range []string{"requested_at", "updated_at"} {
		if !lakeTimePattern.MatchString(desired[column].(string)) {
			t.Errorf("desired row %s = %v, not lake's microsecond time", column, desired[column])
		}
	}

	// 2. lake appends an environment value and a custom domain, with its own SQL.
	zetta.mustExec(t, lakeSetGreeting)
	zetta.mustExec(t, lakeCreateDomain)

	// 3. The operator's sync materializes all three into the LakeEnvironment.
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme}
	syncDeploymentRows(t, reconciler, lake, zetta)
	lakeReadsAll(t, zetta)
	event, err := parseDeploymentRow(map[string]string{"repository": lakeContractRepository, "environment": "production"})
	if err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "lake", Name: deploymentEnvironmentName("prod", event)}
	var environment oramav1alpha1.LakeEnvironment
	if err := kubeClient.Get(ctx, key, &environment); err != nil {
		t.Fatalf("LakeEnvironment: %v", err)
	}
	if len(environment.Spec.Env) != 1 || environment.Spec.Env[0] != (oramav1alpha1.LakeEnvironmentVariable{Name: "GREETING", Value: "hello"}) {
		t.Errorf("spec.env = %+v, want lake's GREETING", environment.Spec.Env)
	}
	if len(environment.Spec.Domains) != 1 || environment.Spec.Domains[0] != (oramav1alpha1.LakeEnvironmentDomain{Host: "shop.example.com", TLS: "acme"}) {
		t.Errorf("spec.domains = %+v", environment.Spec.Domains)
	}
	domains := lakeLatestByKey(t, zetta.mustExec(t, lakeSelectDomains), byDomain)
	if row := domains[lakeContractRepository+"/production/shop.example.com"]; row["status"] != "pending" ||
		!strings.Contains(row["message"].(string), "waiting for the environment") {
		t.Errorf("domain before the environment reconciled = %+v", row)
	}

	// 4. The environment runs: the workload is ready, the Ingress has an address, the DNS record
	// is reconciled.
	environments := &LakeEnvironmentReconciler{Client: kubeClient, Scheme: scheme}
	request := ctrlRequest(client.ObjectKeyFromObject(&environment))
	if _, err := environments.Reconcile(ctx, request); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}
	if err := kubeClient.Get(ctx, key, &environment); err != nil {
		t.Fatal(err)
	}
	workloadNamespace := lakeEnvironmentNamespace(&environment)
	var deployment appsv1.Deployment
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Env; len(got) != 1 || got[0].Name != "GREETING" || got[0].Value != "hello" {
		t.Errorf("workload env = %+v", got)
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	var ingress networkingv1.Ingress
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &ingress); err != nil {
		t.Fatalf("the domain got no Ingress: %v", err)
	}
	if len(ingress.Spec.Rules) != 1 || ingress.Spec.Rules[0].Host != "shop.example.com" {
		t.Errorf("Ingress rules = %+v", ingress.Spec.Rules)
	}
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.10"}}
	if err := kubeClient.Status().Update(ctx, &ingress); err != nil {
		t.Fatal(err)
	}
	if _, err := environments.Reconcile(ctx, request); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}
	dnsKey := types.NamespacedName{Namespace: "lake", Name: environmentDNSRecordName(&environment, "shop.example.com")}
	var dns oramav1alpha1.CloudflareDNSRecord
	if err := kubeClient.Get(ctx, dnsKey, &dns); err != nil {
		t.Fatalf("the domain got no DNS record: %v", err)
	}
	if dns.Spec.Name != "shop.example.com" || dns.Spec.Content != "203.0.113.10" || dns.Spec.SecretRef != "cloudflare" || !metav1.IsControlledBy(&dns, &environment) {
		t.Errorf("DNS record = %+v", dns)
	}
	dns.Status.Ready = true
	if err := kubeClient.Status().Update(ctx, &dns); err != nil {
		t.Fatal(err)
	}
	if _, err := environments.Reconcile(ctx, request); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}

	// 5. The next sync appends the status rows; lake's reads succeed and show them.
	syncDeploymentRows(t, reconciler, lake, zetta)
	deploymentRows, envRows, domainRows := lakeReadsAll(t, zetta)
	current := lakeLatestByKey(t, deploymentRows, byDeploymentID)[record.ID]
	if current["phase"] != oramav1alpha1.LakeEnvironmentPhaseReady || current["namespace"] != workloadNamespace ||
		current["requested_by"] != "lake-admin-token/prod" || current["change"] != desired["change"] ||
		current["updated_at"].(string) <= desired["updated_at"].(string) || !lakeTimePattern.MatchString(current["updated_at"].(string)) {
		t.Errorf("lake shows deployment %+v", current)
	}
	if len(envRows) != 1 {
		t.Errorf("the operator wrote environment_env rows: %+v", envRows)
	}
	domain := lakeLatestByKey(t, domainRows, byDomain)[lakeContractRepository+"/production/shop.example.com"]
	if domain["status"] != "active" || domain["deleted"] != false || domain["tls"] != "acme" ||
		domain["requested_by"] != "user/u-1" || !lakeTimePattern.MatchString(domain["updated_at"].(string)) {
		t.Errorf("lake shows domain %+v", domain)
	}
	deploymentWrites, domainWrites := zetta.insertCount("deployments"), zetta.insertCount("domains")
	syncDeploymentRows(t, reconciler, lake, zetta)
	if zetta.insertCount("deployments") != deploymentWrites || zetta.insertCount("domains") != domainWrites {
		t.Errorf("a steady sync wrote rows: deployments %d -> %d, domains %d -> %d",
			deploymentWrites, zetta.insertCount("deployments"), domainWrites, zetta.insertCount("domains"))
	}

	// 6. lake unsets the value and deletes the domain: both leave the environment.
	zetta.mustExec(t, lakeUnsetGreeting)
	zetta.mustExec(t, lakeDeleteDomain)
	syncDeploymentRows(t, reconciler, lake, zetta)
	if err := kubeClient.Get(ctx, key, &environment); err != nil {
		t.Fatal(err)
	}
	if len(environment.Spec.Env) != 0 || len(environment.Spec.Domains) != 0 {
		t.Errorf("after the tombstones spec.env = %+v, spec.domains = %+v", environment.Spec.Env, environment.Spec.Domains)
	}
	if _, err := environments.Reconcile(ctx, request); err != nil {
		t.Fatalf("environment reconcile: %v", err)
	}
	if err := kubeClient.Get(ctx, dnsKey, &dns); err == nil {
		t.Errorf("the deleted domain's DNS record remains: %+v", dns)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &ingress); err == nil {
		t.Errorf("the Ingress remains with no host left: %+v", ingress.Spec.Rules)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: workloadNamespace, Name: "environment"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Env; len(got) != 0 {
		t.Errorf("workload env after unset = %+v", got)
	}
	_, _, domainRows = lakeReadsAll(t, zetta)
	domain = lakeLatestByKey(t, domainRows, byDomain)[lakeContractRepository+"/production/shop.example.com"]
	if domain["status"] != domainStatusDeleted || domain["deleted"] != true {
		t.Errorf("lake shows the deleted domain as %+v", domain)
	}
	if zetta.authFailures != 0 {
		t.Errorf("Zetta requests without the bearer: %d", zetta.authFailures)
	}
}

// A domain that cannot be applied is answered failed, not applied: a host claimed by another
// environment of the Lake, the Lake's own host, or not a DNS name.
func TestLakeDomainsTheOperatorRefuses(t *testing.T) {
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	row := func(environment, host string) map[string]string {
		return map[string]string{"lake": testLakeHost, "repository": "r", "environment": environment, "host": host,
			"tls": "acme", "status": "pending", "deleted": "false", "updated_at": "2026-10-17T10:00:00.000000Z"}
	}
	plan := planLakeDomains(lake, testLakeHost, []map[string]string{
		row("a", "shop.example.com"),
		row("b", "shop.example.com"),
		row("a", testLakeHost),
		row("a", "Not_A_Host"),
		row("b", "docs.example.com"),
	})
	if got := plan.active[platformEnvironmentKey("r", "a")]; len(got) != 1 || got[0].Host != "shop.example.com" {
		t.Errorf("environment a domains = %+v", got)
	}
	if got := plan.active[platformEnvironmentKey("r", "b")]; len(got) != 1 || got[0].Host != "docs.example.com" {
		t.Errorf("environment b domains = %+v", got)
	}
	for key, want := range map[string]string{
		"r\nb\nshop.example.com": "another environment",
		"r\na\n" + testLakeHost:  "the Lake's own host",
		"r\na\nNot_A_Host":       "not a lowercase DNS name",
	} {
		if !strings.Contains(plan.refused[key], want) {
			t.Errorf("refusal of %q = %q, want %q", key, plan.refused[key], want)
		}
	}
	status, message := domainObservedStatus(row("b", "shop.example.com"), plan.refused["r\nb\nshop.example.com"], nil)
	if status != oramav1alpha1.LakeEnvironmentDomainFailed || message == "" {
		t.Errorf("refused domain status = %q %q", status, message)
	}
}

// A status row is timed after the row it answers even when this clock is behind lake's.
func TestZettaRowTimeAfterOrdersAfterLake(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	if got := zettaRowTimeAfter(now, "2026-10-17T10:00:05.000000Z"); got != "2026-10-17T10:00:05.000001Z" {
		t.Errorf("behind lake's clock: %s", got)
	}
	if got := zettaRowTimeAfter(now, "2026-10-17T09:00:00Z"); got != "2026-10-17T10:00:00.000000Z" {
		t.Errorf("ahead of the row: %s", got)
	}
}
