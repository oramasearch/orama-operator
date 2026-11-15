/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// domainClaimRow is one lake domains row of repository/environment for host, at updatedAt.
func domainClaimRow(repository, environment, host, updatedAt string, deleted bool) map[string]string {
	flag := "false"
	if deleted {
		flag = "true"
	}
	return map[string]string{"lake": testLakeHost, "repository": repository, "environment": environment, "host": host,
		"tls": "acme", "status": "pending", "deleted": flag, "updated_at": updatedAt}
}

func activeHosts(plan lakeDomainPlan, repository, environment string) []string {
	var hosts []string
	for _, domain := range plan.active[platformEnvironmentKey(repository, environment)] {
		hosts = append(hosts, domain.Host)
	}
	return hosts
}

// A host belongs to the first claim by creation time, never to the repository id that sorts first:
// tenant A, whose repository id sorts before tenant B's, cannot take B's domain by claiming it later
// (#776).
func TestLakeDomainBelongsToTheFirstClaim(t *testing.T) {
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	plan := planVerifiedLakeDomains(lake, testLakeHost, []map[string]string{
		domainClaimRow("zzz-tenant-b", "production", "shop.b.com", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("aaa-tenant-a", "production", "shop.b.com", "2026-10-17T11:00:00.000000Z", false),
	}, lakeDomainGuard{})
	if got := activeHosts(plan, "zzz-tenant-b", "production"); len(got) != 1 || got[0] != "shop.b.com" {
		t.Errorf("the first claimant's domains = %v, want shop.b.com", got)
	}
	if got := activeHosts(plan, "aaa-tenant-a", "production"); len(got) != 0 {
		t.Errorf("the later claimant took %v", got)
	}
	if refused := plan.refused["aaa-tenant-a\nproduction\nshop.b.com"]; !strings.Contains(refused, "claimed by another environment") {
		t.Errorf("the later claim's refusal = %q", refused)
	}
}

// A claim's time is its first row since its last tombstone: a later row of the same claim (a tls
// change, the operator's own status answer) does not make it younger, and a claim withdrawn and made
// again is a new claim, behind every claim still live.
func TestLakeDomainClaimTimeIsItsFirstRowSinceItsTombstone(t *testing.T) {
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	updated := domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T12:00:00.000000Z", false)
	updated["tls"] = "provided"
	answered := domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T12:00:00.000001Z", false)
	answered["status"] = "active"
	plan := planVerifiedLakeDomains(lake, testLakeHost, []map[string]string{
		domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T10:00:00Z", false),
		domainClaimRow("aaa", "production", "shop.example.com", "2026-10-17T11:00:00.000000Z", false),
		updated,
		answered,
	}, lakeDomainGuard{})
	if got := activeHosts(plan, "zzz", "production"); len(got) != 1 || got[0] != "shop.example.com" {
		t.Errorf("an updated claim lost its host: zzz = %v, aaa = %v", got, activeHosts(plan, "aaa", "production"))
	}

	// zzz withdraws and claims again: aaa's claim, live all along, is now the first.
	plan = planVerifiedLakeDomains(lake, testLakeHost, []map[string]string{
		domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("aaa", "production", "shop.example.com", "2026-10-17T11:00:00.000000Z", false),
		domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T12:00:00.000000Z", true),
		domainClaimRow("zzz", "production", "shop.example.com", "2026-10-17T13:00:00.000000Z", false),
	}, lakeDomainGuard{})
	if got := activeHosts(plan, "aaa", "production"); len(got) != 1 || got[0] != "shop.example.com" {
		t.Errorf("after zzz withdrew: aaa = %v, zzz = %v", got, activeHosts(plan, "zzz", "production"))
	}

	// Two claims at the same time: the claim's id (repository, environment, host) decides, the same
	// way on every poll.
	plan = planVerifiedLakeDomains(lake, testLakeHost, []map[string]string{
		domainClaimRow("r", "b", "same.example.com", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("r", "a", "same.example.com", "2026-10-17T10:00:00.000000Z", false),
	}, lakeDomainGuard{})
	if got := activeHosts(plan, "r", "a"); len(got) != 1 {
		t.Errorf("a tie is not decided by the claim's id: a = %v, b = %v", got, activeHosts(plan, "r", "b"))
	}
}

// A platform host is never a tenant's domain: the Lake's host, public host and apex host and every
// name beneath them, the apps and preview wildcards, the zone the Lake's DNS token writes in, the
// hosts of every other Lake on the cluster, a host a LakeApp serves and a domain or host another
// Lake's environment serves (#776).
func TestLakeDomainNeverTakesAPlatformHost(t *testing.T) {
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "eu", UID: "lake-uid"},
		Spec: oramav1alpha1.LakeSpec{
			Namespace: "lake", Host: "eu.lake.live", ApexHost: "lake.live",
			PreviewWildcard: "preview.lake.live", Apps: oramav1alpha1.LakeAppsSpec{Wildcard: "*.apps.lake.live"},
			DNSSecretRef: &corev1.LocalObjectReference{Name: "cloudflare-api-token-lake-live"},
		},
	}
	other := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "nonprod"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake-nonprod", Host: "lake-dev.oramadev.net"},
	}
	guard := newLakeDomainGuard(lake, []oramav1alpha1.Lake{*lake, *other},
		[]oramav1alpha1.LakeApp{{
			ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "ws-1"},
			Spec:       oramav1alpha1.LakeAppSpec{LakeRef: "eu", Host: "billing.customer.example"},
		}},
		[]oramav1alpha1.LakeEnvironment{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "lake-nonprod"},
				Spec: oramav1alpha1.LakeEnvironmentSpec{LakeRef: "nonprod", Repository: "r", Environment: "e",
					Host: "named.other.example", Domains: []oramav1alpha1.LakeEnvironmentDomain{{Host: "taken.customer.example", TLS: "acme"}}},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "lake"},
				Spec: oramav1alpha1.LakeEnvironmentSpec{LakeRef: "eu", Repository: "own", Environment: "e",
					Host: "named.mine.example", Domains: []oramav1alpha1.LakeEnvironmentDomain{{Host: "kept.customer.example", TLS: "acme"}}},
			},
		})
	refusedHosts := []string{
		"eu.lake.live", "login.eu.lake.live", // the Lake's host and beneath it
		"lake.live", "status.lake.live", "login.lake.live", // the apex host, the zone
		"shop.apps.lake.live", "apps.lake.live", // the apps wildcard
		"x.preview.lake.live",                                // the preview wildcard
		"lake-dev.oramadev.net", "app.lake-dev.oramadev.net", // another Lake on the cluster
		"billing.customer.example",                      // a LakeApp's host
		"taken.customer.example", "named.other.example", // another Lake's environment
		"named.mine.example", // a named environment host of this Lake
	}
	rows := make([]map[string]string, 0, len(refusedHosts)+1)
	for _, host := range refusedHosts {
		rows = append(rows, domainClaimRow("tenant", "production", host, "2026-10-17T10:00:00.000000Z", false))
	}
	rows = append(rows,
		domainClaimRow("tenant", "production", "shop.customer.example", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("own", "e", "kept.customer.example", "2026-10-17T10:00:00.000000Z", false))
	plan := planVerifiedLakeDomains(lake, "eu.lake.live", rows, guard)
	for _, host := range refusedHosts {
		if refused := plan.refused["tenant\nproduction\n"+host]; refused == "" {
			t.Errorf("%s was not refused", host)
		}
	}
	if got := activeHosts(plan, "tenant", "production"); len(got) != 1 || got[0] != "shop.customer.example" {
		t.Errorf("the tenant's domains = %v, want only shop.customer.example", got)
	}
	// A domain this Lake's own environment already serves is not refused as "taken": it is that
	// environment's claim, decided by the first-claim rule.
	if got := activeHosts(plan, "own", "e"); len(got) != 1 || got[0] != "kept.customer.example" {
		t.Errorf("this Lake's environment lost its own domain: %v (%q)", got, plan.refused["own\ne\nkept.customer.example"])
	}
}

// A certificate another app or environment owns is never copied to a second owner: the second one
// gets no certificate for the host, rather than the first one's key (#776).
func TestLakeHostCertificateIsNeverSharedAcrossOwners(t *testing.T) {
	s := newLakeScheme(t)
	ctx := context.Background()
	lake, secret := originTLSLake()
	owner := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "lake", UID: "owner-uid"}, Spec: oramav1alpha1.LakeAppSpec{LakeRef: "shared"}}
	claimant := &oramav1alpha1.LakeApp{ObjectMeta: metav1.ObjectMeta{Name: "claimant", Namespace: "lake", UID: "claimant-uid"}, Spec: oramav1alpha1.LakeAppSpec{LakeRef: "shared"}}
	issued := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shop.example.com-tls", Namespace: "lake"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("owner-cert"), corev1.TLSPrivateKeyKey: []byte("owner-key")},
	}
	policy := &oramav1alpha1.EdgeZonePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-shared-edge", Namespace: "lake"},
		Status:     oramav1alpha1.EdgeZonePolicyStatus{OriginEncryption: "strict", AuthenticatedOriginPulls: true, OriginClientCAPEM: cloudflareOriginPullCA},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(lake, secret, owner, claimant, issued, policy).
		WithStatusSubresource(&networkingv1.Ingress{}).Build()
	r := &LakeAppReconciler{Client: c, Scheme: s}

	if err := r.reconcileLakeAppIngress(ctx, lake, owner, "ws-owner", "shop.example.com"); err != nil {
		t.Fatalf("owner ingress: %v", err)
	}
	if err := r.reconcileLakeAppIngress(ctx, lake, claimant, "ws-claimant", "shop.example.com"); err != nil {
		t.Fatalf("claimant ingress: %v", err)
	}
	ownerCopy := tlsSecretsByHost(getIngress(t, c, "ws-owner", lakeAppWorkloadObjectName(owner)))["shop.example.com"]
	var copied corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ws-owner", Name: ownerCopy}, &copied); err != nil {
		t.Fatalf("the owner's own copy: %v", err)
	}
	claimantCopy := tlsSecretsByHost(getIngress(t, c, "ws-claimant", lakeAppWorkloadObjectName(claimant)))["shop.example.com"]
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ws-claimant", Name: claimantCopy}, &copied); !apierrors.IsNotFound(err) {
		t.Errorf("the owner's certificate was copied to another tenant's namespace: %v %q", err, copied.Data[corev1.TLSPrivateKeyKey])
	}
	// The owner's Certificate is still the owner's.
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "lake", Name: "shop.example.com-tls"}, certificate); err != nil {
		t.Fatal(err)
	}
	if refs := certificate.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "owner" {
		t.Errorf("certificate owners = %+v", refs)
	}
}

// planVerifiedLakeDomains plans rows as if lake had verified a claim of every live row: the tests
// of the other rules (first claim, platform hosts) start from verified claims.
func planVerifiedLakeDomains(lake *oramav1alpha1.Lake, host string, rows []map[string]string, guard lakeDomainGuard) lakeDomainPlan {
	return planLakeDomains(lake, host, rows, guard, newLakeVerifiedDomains(verifiedClaimsFor(rows)))
}

// verifiedClaimsFor is a verified claim of each live row's host by its repository and environment.
func verifiedClaimsFor(rows []map[string]string) []LakeDomainClaim {
	var claims []LakeDomainClaim
	for _, row := range rows {
		if parseDeploymentBool(row["deleted"]) || row["host"] == "" {
			continue
		}
		claims = append(claims, LakeDomainClaim{Host: row["host"], Environment: row["environment"], RepositoryID: row["repository"]})
	}
	return claims
}

// A domain row is routed only with a verified claim of its host by its repository and environment
// (lake#949): no claim, a claim of another environment or another repository, is refused with the
// reason, and a refused row never takes the host from the verified one, even when it is older.
func TestLakeDomainWithoutAVerifiedClaimIsRefused(t *testing.T) {
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	rows := []map[string]string{
		domainClaimRow("tenant-a", "production", "shop.example.com", "2026-10-17T09:00:00.000000Z", false),
		domainClaimRow("tenant-b", "production", "shop.example.com", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("tenant-b", "staging", "staging.example.com", "2026-10-17T10:00:00.000000Z", false),
		domainClaimRow("tenant-b", "production", "unclaimed.example.com", "2026-10-17T10:00:00.000000Z", false),
	}
	verified := newLakeVerifiedDomains([]LakeDomainClaim{
		{Host: "shop.example.com", Environment: "production", RepositoryID: "tenant-b"},
		{Host: "staging.example.com", Environment: "production", RepositoryID: "tenant-b"},
	})
	plan := planLakeDomains(lake, testLakeHost, rows, lakeDomainGuard{}, verified)
	if got := activeHosts(plan, "tenant-b", "production"); len(got) != 1 || got[0] != "shop.example.com" {
		t.Errorf("the verified claim's domains = %v, want shop.example.com", got)
	}
	for _, key := range []string{
		"tenant-a\nproduction\nshop.example.com",
		"tenant-b\nstaging\nstaging.example.com",
		"tenant-b\nproduction\nunclaimed.example.com",
	} {
		if refused := plan.refused[key]; !strings.Contains(refused, "has no verified claim") || !strings.Contains(refused, "_lake-challenge.") {
			t.Errorf("%q refusal = %q", key, refused)
		}
	}
	// Without any verified claim (a lake schema before domain_claims) nothing is routed.
	plan = planLakeDomains(lake, testLakeHost, rows, lakeDomainGuard{}, nil)
	if len(plan.active) != 0 {
		t.Errorf("routed without a verified claim: %+v", plan.active)
	}
}

// A domain row names the repository by the deployment's own value, lake's id or liquid's (lake#805):
// a claim allows the host under either, whatever the case of the liquid id.
func TestLakeDomainClaimMatchesTheLiquidID(t *testing.T) {
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	rows := []map[string]string{
		domainClaimRow("00112233445566778899AABBCCDDEEFF", "production", "shop.example.com", "2026-10-17T09:00:00.000000Z", false),
	}
	verified := newLakeVerifiedDomains([]LakeDomainClaim{{
		Host: "shop.example.com", Environment: "production",
		RepositoryID: "0f8fad5b-d9cb-469f-a165-70867728950e", LiquidID: "00112233445566778899aabbccddeeff",
	}})
	plan := planLakeDomains(lake, testLakeHost, rows, lakeDomainGuard{}, verified)
	if got := activeHosts(plan, "00112233445566778899AABBCCDDEEFF", "production"); len(got) != 1 {
		t.Errorf("the liquid id's domains = %v (%q)", got, plan.refused["00112233445566778899AABBCCDDEEFF\nproduction\nshop.example.com"])
	}
}

// The sync routes a domain once its claim is verified, answers an unverified one failed with the
// reason, keeps every environment's domains unchanged when the claims cannot be read, and refuses
// every domain on a Lake whose schema has no domain_claims table (lake#949).
func TestLakeDomainSyncActsOnlyOnVerifiedClaims(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeEnvironmentScheme(t)
	lake := testDeploymentLake(oramav1alpha1.LakeSpec{})
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).
		WithStatusSubresource(&oramav1alpha1.LakeEnvironment{}).Build()
	var claims []LakeDomainClaim
	var claimsErr error
	reconciler := &OramaZettaClusterReconciler{Client: kubeClient, Scheme: scheme,
		DomainClaims: func(context.Context, *oramav1alpha1.Lake) ([]LakeDomainClaim, error) { return claims, claimsErr }}
	desired := desiredTestRow(nil)
	zetta := newFakeZetta(t, "test-token")
	insertTestRow(t, zetta, "deployments", desired)
	insertTestRow(t, zetta, "domains", map[string]string{
		"lake": testLakeHost, "repository": "repo-1", "environment": "production", "host": "shop.example.com",
		"tls": "acme", "status": "pending", "message": "", "deleted": "false", "requested_by": "user/u-1",
		"updated_at": "2026-09-05T10:00:01.000000Z", "ordering_token": "e4eaaaf2-d142-11e1-b3e4-080027620cdd",
	})
	event, err := parseDeploymentRow(desired)
	if err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "lake", Name: deploymentEnvironmentName(lake.Name, event)}
	domainsOf := func() []oramav1alpha1.LakeEnvironmentDomain {
		t.Helper()
		var environment oramav1alpha1.LakeEnvironment
		if err := kubeClient.Get(ctx, key, &environment); err != nil {
			t.Fatalf("LakeEnvironment: %v", err)
		}
		return environment.Spec.Domains
	}
	latestDomainStatus := func() (string, string) {
		t.Helper()
		rows := zetta.rows("orama_control", "domains")
		latest := rows[len(rows)-1]
		return coerceSQLString(latest["status"]), coerceSQLString(latest["message"])
	}

	// 1. Not verified: the environment gets no domain and the row is answered failed, with why.
	syncDeploymentRows(t, reconciler, lake, zetta)
	if got := domainsOf(); len(got) != 0 {
		t.Errorf("an unverified domain was routed: %+v", got)
	}
	if status, message := latestDomainStatus(); status != oramav1alpha1.LakeEnvironmentDomainFailed || !strings.Contains(message, "has no verified claim") {
		t.Errorf("unverified domain answered %q %q", status, message)
	}

	// 2. Verified: the domain is routed.
	claims = []LakeDomainClaim{{Host: "shop.example.com", Environment: "production", RepositoryID: "repo-1"}}
	syncDeploymentRows(t, reconciler, lake, zetta)
	if got := domainsOf(); len(got) != 1 || got[0].Host != "shop.example.com" {
		t.Fatalf("the verified domain = %+v", got)
	}

	// 3. The claims cannot be read: nothing changes, nothing is answered.
	claims, claimsErr = nil, errors.New("connection refused")
	before := len(zetta.rows("orama_control", "domains"))
	syncDeploymentRows(t, reconciler, lake, zetta)
	if got := domainsOf(); len(got) != 1 || got[0].Host != "shop.example.com" {
		t.Errorf("an unreadable claims table changed the domains: %+v", got)
	}
	if after := len(zetta.rows("orama_control", "domains")); after != before {
		t.Errorf("domain status rows written without the claims: %d -> %d", before, after)
	}

	// 4. No domain_claims table: every domain is refused.
	claimsErr = errDomainClaimsTableMissing
	syncDeploymentRows(t, reconciler, lake, zetta)
	if got := domainsOf(); len(got) != 0 {
		t.Errorf("routed on a schema without domain_claims: %+v", got)
	}
}
