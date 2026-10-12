/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func scheduledImportsOC() *oramav1alpha1.OramaCloudV1 {
	return fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.AnalyticsProcessor.Import.Enabled = true
		o.Spec.Components.Engine.Reader.Replicas = 2
	})
}

func TestCloudV1JWTAndFirebase(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.JWTIssuer = "https://app.orama.ing"
		o.Spec.IDP = oramav1alpha1.IDPSpec{Provider: "firebase", ConfigSecretRef: &corev1.LocalObjectReference{Name: "firebase-server"}}
		o.Spec.Components.Engine.Writer.JWT = &oramav1alpha1.WriterJWTSpec{
			JWKSURL: "https://app.orama.ing/api/.well-known/jwks.json", Audience: "https://writer.prod2.cluster.orama.ing",
		}
	})
	cl, r := reconcileOCV1(t, oc)
	var cfg struct {
		Writer struct {
			JWT struct {
				Providers []struct {
					JWKSURL            string `json:"jwks_url"`
					Issuers, Audiences []string
				}
			}
		} `json:"writer_side"`
	}
	if err := yaml.Unmarshal([]byte(oramacoreConfigYAML(oc)), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Writer.JWT.Providers
	if len(p) != 1 || p[0].JWKSURL != oc.Spec.Components.Engine.Writer.JWT.JWKSURL || !slices.Equal(p[0].Issuers, []string{oc.Spec.JWTIssuer}) || !slices.Equal(p[0].Audiences, []string{oc.Spec.Components.Engine.Writer.JWT.Audience}) {
		t.Fatalf("writer JWT provider = %+v", p)
	}
	if got := getObj(t, cl, "v1-config", &corev1.ConfigMap{}).Data["JWT_ISSUER"]; got != oc.Spec.JWTIssuer {
		t.Fatalf("issuer = %q", got)
	}
	dep := getObj(t, cl, "v1-dashboard", &appsv1.Deployment{})
	for _, key := range []string{"FIREBASE_PROJECT_ID", "FIREBASE_CLIENT_EMAIL", "FIREBASE_PRIVATE_KEY"} {
		if !slices.ContainsFunc(dep.Spec.Template.Spec.Containers[0].Env, func(e corev1.EnvVar) bool {
			return e.Name == key && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "firebase-server" && e.ValueFrom.SecretKeyRef.Key == key && e.ValueFrom.SecretKeyRef.Optional == nil
		}) {
			t.Errorf("missing required Secret reference for %s", key)
		}
	}
	writer := getObj(t, cl, "v1-writer", &appsv1.StatefulSet{})
	hash := writer.Spec.Template.Annotations["orama.cloud/config-hash"]
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"] != hash {
		t.Error("unchanged JWT config changed rollout hash")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.JWTIssuer = "new-issuer" })
	if getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}).Spec.Template.Annotations["orama.cloud/config-hash"] == hash {
		t.Error("changed issuer did not roll engine config")
	}

	defaults := newOCV1(nil)
	if jwtIssuer(defaults) != "orama-cloud" || strings.Contains(oramacoreConfigYAML(defaults), "  jwt:") {
		t.Error("default JWT behavior changed")
	}
	if _, exists := componentSecretData("dashboard")["FIREBASE_SERVICE_ACCOUNT"]; exists {
		t.Error("nonfunctional Firebase dummy is still seeded")
	}
	oc.Spec.IDP.ConfigSecretRef = nil
	if err := r.reconcileDashboard(context.Background(), oc); err == nil {
		t.Error("Firebase without a Secret reference must fail visibly")
	}
}

func TestCloudV1JWTRecoversPublicSecretWithoutRotation(t *testing.T) {
	oc := newOCV1(nil)
	scheme := ocv1TestScheme(t)
	jwt := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-jwt", Namespace: oc.Namespace},
		Data: map[string][]byte{"JWT_PRIVATE_KEY": []byte("preserved-private"), "JWT_PUBLIC_KEY": []byte("preserved-public")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(jwt).Build()
	r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
	if err := r.ensureJWTSecrets(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	pub := getObj(t, cl, "v1-jwt-pub", &corev1.Secret{})
	if len(pub.StringData) != 1 || pub.StringData["JWT_PUBLIC_KEY"] != "preserved-public" || !metav1.IsControlledBy(pub, oc) {
		t.Error("public-only Secret did not recover from preserved pair")
	}
	if getObj(t, cl, "v1-jwt", &corev1.Secret{}).ResourceVersion != jwt.ResourceVersion {
		t.Error("recovering public Secret changed signing pair")
	}
	version := pub.ResourceVersion
	if err := r.ensureJWTSecrets(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if getObj(t, cl, pub.Name, &corev1.Secret{}).ResourceVersion != version {
		t.Error("steady key recovery rewrote Secret")
	}
}

func TestCloudV1PublicIngressAndDNSLifecycle(t *testing.T) {
	oc := fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Writer.Host = "writer.prod2.cluster.orama.ing"
		o.Spec.Components.Engine.Reader.Host = "collections.orama.ing"
		o.Spec.Components.Dashboard.Host = "app.orama.ing"
		o.Spec.Cloudflare = oramav1alpha1.CloudflareSpec{Enabled: true, ManageDNS: true, APITokenSecretRef: &corev1.LocalObjectReference{Name: "cf"}}
	})
	cl, r := reconcileOCV1(t, oc)
	ctx := context.Background()
	for _, component := range []string{"dashboard", "writer", "reader"} {
		name := "v1-" + component
		ing := getObj(t, cl, name, &networkingv1.Ingress{})
		port := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number
		if component == "reader" && port != 80 || component == "writer" && port != 8080 {
			t.Errorf("%s backend port = %d", component, port)
		}
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.0.2.10"}}
		if err := cl.Status().Update(ctx, ing); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileCloudflare(ctx, oc); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"dashboard", "writer", "reader"} {
		dns := getObj(t, cl, "v1-"+component+"-dns", &oramav1alpha1.CloudflareDNSRecord{})
		if dns.Spec.Content != "192.0.2.10" || !dns.Spec.Proxied || dns.Spec.SecretRef != "cf" || !metav1.IsControlledBy(dns, oc) {
			t.Errorf("DNS = %+v", dns)
		}
	}
	// Host edits update the same child CR; clearing a host deletes its owned resources.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "reads.example.com" })
	if getObj(t, cl, "v1-reader-dns", &oramav1alpha1.CloudflareDNSRecord{}).Spec.Name != "reads.example.com" {
		t.Error("DNS did not follow host edit")
	}
	// A provisioned DNS name change finalizes its old child before reusing the resource name.
	old := getObj(t, cl, "v1-reader-dns", &oramav1alpha1.CloudflareDNSRecord{})
	old.Status.RecordID = "existing-record"
	if err := cl.Status().Update(ctx, old); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Host = "new-reads.example.com" })
	if !absent(t, cl, "v1-reader-dns", &oramav1alpha1.CloudflareDNSRecord{}) {
		t.Error("provisioned old DNS name was overwritten before finalization")
	}
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1-reader-dns", &oramav1alpha1.CloudflareDNSRecord{}).Spec.Name != "new-reads.example.com" {
		t.Error("new DNS name was not recreated after cleanup")
	}

	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.Host = ""
		o.Spec.Cloudflare.ManageDNS = false
	})
	if !absent(t, cl, "v1-reader", &networkingv1.Ingress{}) {
		t.Error("cleared reader host retained Ingress")
	}
	for _, component := range []string{"dashboard", "writer", "reader"} {
		if !absent(t, cl, "v1-"+component+"-dns", &oramav1alpha1.CloudflareDNSRecord{}) {
			t.Errorf("disabled DNS retained %s", component)
		}
	}
}

func TestCloudV1ScheduledImportsAndScaling(t *testing.T) {
	oc := scheduledImportsOC()
	oc.Spec.Placement.NodeSelector = map[string]string{"agentpool": "prod2"}
	oc.Spec.Placement.Tolerations = []corev1.Toleration{{Key: "agentpool", Value: "prod2", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}}
	oc.Spec.Images.PullSecretName = "private-images"
	cl, r := reconcileOCV1(t, oc)
	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	if sts.Spec.ServiceName != "v1-reader-headless" || getObj(t, cl, "v1-reader-headless", &corev1.Service{}).Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatal("ordinal DNS lacks headless governing Service")
	}
	cm := getObj(t, cl, "v1-analytics-processor-config", &corev1.ConfigMap{})
	for _, ordinal := range []string{"0", "1"} {
		if !strings.Contains(cm.Data["config.yaml"], "http://v1-reader-"+ordinal+".v1-reader-headless.orama-system.svc:8080/v1/analytics") {
			t.Errorf("missing ordinal %s", ordinal)
		}
	}
	if strings.Contains(cm.Data["config.yaml"], "http://v1-reader/v1/analytics") {
		t.Error("scheduled imports still load-balance reader polling")
	}
	cron := getObj(t, cl, "v1-analytics-import", &batchv1.CronJob{})
	pod := cron.Spec.JobTemplate.Spec.Template.Spec
	if cron.Spec.Schedule != "*/5 * * * *" || cron.Spec.TimeZone == nil || *cron.Spec.TimeZone != "Etc/UTC" || cron.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent || !metav1.IsControlledBy(cron, oc) {
		t.Errorf("schedule = %+v", cron.Spec)
	}
	if pod.NodeSelector["agentpool"] != "prod2" || len(pod.Tolerations) != 1 || pod.ImagePullSecrets[0].Name != "private-images" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("trigger placement/identity = %+v", pod)
	}
	if !slices.Contains(pod.Containers[0].Args, "POST") || !slices.Contains(pod.Containers[0].Args, "http://v1-analytics-processor:8080/import") || !slices.Contains(pod.Containers[0].Args, "--fail") || *cron.Spec.JobTemplate.Spec.BackoffLimit != 0 {
		t.Error("trigger must POST to reachable Service, fail on HTTP errors and avoid duplicate retries")
	}
	// Simulate native defaults on the returned Job template, then verify steady reconciliation.
	cron.Spec.JobTemplate.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	cron.Spec.JobTemplate.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
	cron.Spec.JobTemplate.Spec.Template.Spec.TerminationGracePeriodSeconds = int64Ptr(30)
	if err := cl.Update(context.Background(), cron); err != nil {
		t.Fatal(err)
	}
	cronVersion := cron.ResourceVersion
	dep := getObj(t, cl, "v1-analytics-processor", &appsv1.Deployment{})
	hash := dep.Spec.Template.Annotations["orama.cloud/config-hash"]
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1-analytics-import", &batchv1.CronJob{}).ResourceVersion != cronVersion {
		t.Error("steady schedule rewrote defaulted CronJob")
	}
	if getObj(t, cl, "v1-analytics-processor", &appsv1.Deployment{}).ResourceVersion != dep.ResourceVersion {
		t.Error("steady import config rewrote Deployment")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = 3 })
	if !strings.Contains(getObj(t, cl, cm.Name, &corev1.ConfigMap{}).Data["config.yaml"], "v1-reader-2.") || getObj(t, cl, dep.Name, &appsv1.Deployment{}).Spec.Template.Annotations["orama.cloud/config-hash"] == hash {
		t.Error("reader scale-up did not update and roll processor config")
	}
	// Once HPA owns replicas, polling follows the StatefulSet's actual desired count.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.HPA = &oramav1alpha1.HPASpec{MinReplicas: 2, MaxReplicas: 5}
	})
	sts = getObj(t, cl, sts.Name, &appsv1.StatefulSet{})
	sts.Spec.Replicas = int32Ptr(4)
	if err := cl.Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, cl, r, nil)
	if !strings.Contains(getObj(t, cl, cm.Name, &corev1.ConfigMap{}).Data["config.yaml"], "v1-reader-3.") {
		t.Error("processor missed HPA reader ordinal")
	}
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Import.Enabled = false })
	if !absent(t, cl, "v1-analytics-import", &batchv1.CronJob{}) || absent(t, cl, "v1-reader-headless", &corev1.Service{}) || getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.ServiceName != "v1-reader-headless" {
		t.Error("disabling imports must remove schedule and preserve immutable reader networking")
	}
}

func TestCloudV1LegacyReaderImportPrerequisite(t *testing.T) {
	oc := fullStackOCV1(nil)
	cl, r := reconcileOCV1(t, oc)
	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	if sts.Spec.ServiceName != "v1-reader" || !absent(t, cl, "v1-reader-headless", &corev1.Service{}) || !absent(t, cl, "v1-analytics-import", &batchv1.CronJob{}) {
		t.Fatal("ordinary CR changed reader topology or gained schedule")
	}
	oc.Spec.Components.AnalyticsProcessor.Import.Enabled = true
	if err := r.reconcileReaders(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}).Spec.ServiceName != "v1-reader" {
		t.Error("legacy immutable serviceName was changed")
	}
	if err := r.reconcileAnalyticsProcessor(context.Background(), oc); err == nil || !strings.Contains(err.Error(), "separately reviewed migration") {
		t.Fatalf("legacy prerequisite error = %v", err)
	}
	if !absent(t, cl, "v1-analytics-import", &batchv1.CronJob{}) {
		t.Error("legacy reader topology gained successful-looking schedule")
	}
}

func TestCloudV1ImportPrerequisitesRemoveSchedule(t *testing.T) {
	for _, field := range []string{"engine", "analytics", "controlDB", "schedule", "processor"} {
		t.Run(field, func(t *testing.T) {
			oc := scheduledImportsOC()
			cl, r := reconcileOCV1(t, oc)
			switch field {
			case "engine":
				oc.Spec.Components.Engine.Enabled = false
			case "analytics":
				oc.Spec.Components.Analytics.Enabled = false
			case "controlDB":
				oc.Spec.Components.ControlDB.Enabled = false
			case "schedule":
				oc.Spec.Components.AnalyticsProcessor.Import.Schedule = "not cron"
			case "processor":
				oc.Spec.Components.AnalyticsProcessor.Enabled = false
			}
			err := r.reconcileAnalyticsProcessor(context.Background(), oc)
			if (err == nil) != (field == "processor") {
				t.Errorf("prerequisite %s: %v", field, err)
			}
			if !absent(t, cl, "v1-analytics-import", &batchv1.CronJob{}) {
				t.Error("invalid/disabled prerequisite left schedule running")
			}
		})
	}
}

func TestCloudV1ExternalClickHouseSecretReferences(t *testing.T) {
	oc := scheduledImportsOC()
	oc.Spec.Components.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "external", Endpoint: "https://clickhouse.example.com:8443", CredentialsSecretRef: &corev1.LocalObjectReference{Name: "external-clickhouse"}}
	cl, _ := reconcileOCV1(t, oc)
	if !absent(t, cl, "v1-analytics", &appsv1.StatefulSet{}) {
		t.Error("external ClickHouse created an in-cluster store")
	}
	dep := getObj(t, cl, "v1-analytics-processor", &appsv1.Deployment{})
	for _, key := range []string{"CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"} {
		if !slices.ContainsFunc(dep.Spec.Template.Spec.InitContainers[0].Env, func(e corev1.EnvVar) bool {
			return e.Name == key && e.ValueFrom.SecretKeyRef.Name == "external-clickhouse"
		}) {
			t.Errorf("missing external %s", key)
		}
	}
	if cfg := getObj(t, cl, "v1-analytics-processor-config", &corev1.ConfigMap{}).Data["config.yaml"]; !strings.Contains(cfg, oc.Spec.Components.Analytics.Endpoint) || !strings.Contains(cfg, "__CLICKHOUSE_USER__") {
		t.Error("external config failed to retain endpoint and credential placeholders")
	}

	// No Secret values are fetched to render either workload; missing keys block startup through required refs.
	s := ocv1TestScheme(t)
	r := &OramaCloudV1Reconciler{Scheme: s, Client: fake.NewClientBuilder().WithScheme(s).Build()}
	oc.Spec.Components.Analytics.CredentialsSecretRef = nil
	if err := r.reconcileDashboard(context.Background(), oc); err == nil {
		t.Error("external dashboard without credential reference must fail")
	}
}
