/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	cronValue   = "cron-value-0001"
	canaryKey   = "p_canary-value-0002"
	canaryImage = "oramasearch/oramacore-test-e2e@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// cronOC is a dashboard reading Secret dash-env, with the crons enabled and suspend left unset.
func cronOC(mut func(*oramav1alpha1.DashboardSpec)) *oramav1alpha1.OramaCloudV1 {
	return dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dash-env")}
		d.Crons.Enabled = true
		if mut != nil {
			mut(d)
		}
	})
}

func dashEnvSecret() *corev1.Secret {
	return secretOf("dash-env", map[string]string{"CRON_SECRET": cronValue})
}

func canarySecret() *corev1.Secret {
	return secretOf("canary", map[string]string{"collectionID": "col-1", "indexID": "idx-1", "apiKey": canaryKey})
}

func withCanary(d *oramav1alpha1.DashboardSpec) {
	d.Crons.E2ECanary = &oramav1alpha1.E2ECanarySpec{Image: canaryImage, SecretRef: corev1.LocalObjectReference{Name: "canary"}}
}

// cronJobs is every CronJob in the namespace, by name.
func (h *engineHarness) cronJobs() map[string]batchv1.CronJob {
	h.t.Helper()
	var list batchv1.CronJobList
	if err := h.cl.List(context.Background(), &list, client.InNamespace("orama-system")); err != nil {
		h.t.Fatal(err)
	}
	out := map[string]batchv1.CronJob{}
	for _, cj := range list.Items {
		out[cj.Name] = cj
	}
	return out
}

func (h *engineHarness) mustFail(want string) error {
	h.t.Helper()
	err := h.run()
	if err == nil || !strings.Contains(err.Error(), want) {
		h.t.Fatalf("Reconcile error = %v, want one containing %q", err, want)
	}
	return err
}

var dashboardCronNames = []string{"v1-dashboard-cron-consumption-sync", "v1-dashboard-cron-crawler", "v1-dashboard-cron-index-gc"}

func cronNames(m map[string]batchv1.CronJob) []string {
	var names []string
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func TestDashboardCronsRenderProd1Schedules(t *testing.T) {
	oc := cronOC(nil)
	oc.Spec.Images.PullSecretName = "regcred"
	h := newEngineHarness(t, oc, dashEnvSecret())
	h.mustRun()

	crons := h.cronJobs()
	if got := cronNames(crons); !reflect.DeepEqual(got, dashboardCronNames) {
		t.Fatalf("CronJobs = %v, want %v", got, dashboardCronNames)
	}
	want := map[string][2]string{
		"v1-dashboard-cron-crawler":          {"*/1 * * * *", "http://v1-dashboard:3000/api/private/v1/run-crawler-cron"},
		"v1-dashboard-cron-index-gc":         {"0 * * * *", "http://v1-dashboard:3000/api/private/v1/run-index-gc-cron"},
		"v1-dashboard-cron-consumption-sync": {"0 */6 * * *", "http://v1-dashboard:3000/api/billing/queue-consumption-sync"},
	}
	for name, w := range want {
		cj := crons[name]
		s, job := cj.Spec, cj.Spec.JobTemplate.Spec
		pod := job.Template.Spec
		if s.Schedule != w[0] || *s.TimeZone != "Etc/UTC" || !*s.Suspend || s.ConcurrencyPolicy != batchv1.ForbidConcurrent ||
			*s.StartingDeadlineSeconds != 60 || *s.SuccessfulJobsHistoryLimit != 1 || *s.FailedJobsHistoryLimit != 3 {
			t.Errorf("%s: CronJob policy %+v", name, s)
		}
		if *job.BackoffLimit != 0 || *job.ActiveDeadlineSeconds != 330 || *job.TTLSecondsAfterFinished != 3600 {
			t.Errorf("%s: Job policy backoff=%d deadline=%d ttl=%d", name, *job.BackoffLimit, *job.ActiveDeadlineSeconds, *job.TTLSecondsAfterFinished)
		}
		if pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken ||
			!reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "regcred"}}) {
			t.Errorf("%s: pod %+v", name, pod)
		}
		for _, lbls := range []map[string]string{cj.Labels, job.Template.Labels} {
			if !reflect.DeepEqual(lbls, labelsFor(oc, "dashboard-crons")) {
				t.Errorf("%s: labels %v", name, lbls)
			}
		}
		if !metav1.IsControlledBy(&cj, oc) {
			t.Errorf("%s: not controlled by the CR (owners %v)", name, cj.OwnerReferences)
		}
		if len(pod.Containers) != 1 {
			t.Fatalf("%s: %d containers", name, len(pod.Containers))
		}
		c := pod.Containers[0]
		if c.Image != pinnedCurlImage || len(c.Command) != 3 || c.Command[0] != "/bin/sh" || c.Command[2] != dashboardCronScript(w[1]) {
			t.Errorf("%s: image %q command %q", name, c.Image, c.Command)
		}
		wantEnv := []corev1.EnvVar{secretKeyEnv("CRON_SECRET", "dash-env", "CRON_SECRET")}
		if !reflect.DeepEqual(c.Env, wantEnv) || len(c.EnvFrom) != 0 {
			t.Errorf("%s: env %+v envFrom %+v", name, c.Env, c.EnvFrom)
		}
		wantResources := *resourceReqs("32Mi", "10m", "64Mi")
		wantResources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("100Mi")
		wantResources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		if !reflect.DeepEqual(c.Resources, wantResources) {
			t.Errorf("%s: resources %+v", name, c.Resources)
		}
		sc := c.SecurityContext
		if !*sc.RunAsNonRoot || *sc.RunAsUser != 100 || *sc.RunAsGroup != 101 || !*sc.ReadOnlyRootFilesystem ||
			*sc.AllowPrivilegeEscalation || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
			sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: securityContext %+v", name, sc)
		}
		b, _ := json.Marshal(cj)
		if strings.Contains(string(b), cronValue) {
			t.Errorf("%s: the CRON_SECRET value is in the CronJob", name)
		}
	}
}

func TestDashboardCronsImageOverrideIsUsedAsIs(t *testing.T) {
	oc := cronOC(nil)
	oc.Spec.Images.Registry, oc.Spec.Images.Tag = "example.io/mirror", "v9"
	oc.Spec.Images.Overrides = map[string]string{"dashboard-crons": "example.io/curl@sha256:" + strings.Repeat("a", 64)}
	h := newEngineHarness(t, oc, dashEnvSecret())
	h.mustRun()
	for name, cj := range h.cronJobs() {
		if got := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image; got != oc.Spec.Images.Overrides["dashboard-crons"] {
			t.Errorf("%s: image %q", name, got)
		}
	}
}

func TestDashboardCronsSuspend(t *testing.T) {
	h := newEngineHarness(t, cronOC(func(d *oramav1alpha1.DashboardSpec) { d.Crons.Suspend = boolPtr(false) }), dashEnvSecret())
	h.mustRun()
	h.mustRun() // an explicit false survives the next reconcile
	for name, cj := range h.cronJobs() {
		if *cj.Spec.Suspend {
			t.Errorf("%s: suspended, want running", name)
		}
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Crons.Suspend = boolPtr(true) })
	h.mustRun()
	for name, cj := range h.cronJobs() {
		if !*cj.Spec.Suspend {
			t.Errorf("%s: running, want suspended", name)
		}
	}
}

func TestDashboardCronsOmittedCreatesNone(t *testing.T) {
	h := newEngineHarness(t, dashOC(func(d *oramav1alpha1.DashboardSpec) { d.EnvFrom = []corev1.EnvFromSource{secretSource("dash-env")} }), dashEnvSecret())
	h.mustRun()
	if crons := h.cronJobs(); len(crons) != 0 {
		t.Fatalf("CronJobs %v, want none", cronNames(crons))
	}
}

func TestDashboardCronsSteadyReconcileWritesNothing(t *testing.T) {
	h := newEngineHarness(t, cronOC(withCanary), dashEnvSecret(), canarySecret())
	h.mustRun()
	before := map[string]string{}
	for name, cj := range h.cronJobs() {
		before[name] = cj.ResourceVersion
	}
	h.mustRun()
	for name, cj := range h.cronJobs() {
		if cj.ResourceVersion != before[name] {
			t.Errorf("%s rewritten by a steady reconcile", name)
		}
	}
}

// foreignCronJob carries the dashboard crons' labels but no controller (or another one).
func foreignCronJob(name string, owner *metav1.OwnerReference) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orama-system", Labels: labelsFor(newOCV1(nil), "dashboard-crons")},
		Spec: batchv1.CronJobSpec{Schedule: "5 5 * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
		}}}},
	}
	if owner != nil {
		cj.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return cj
}

func TestDashboardCronsDisableDeletesOnlyOwned(t *testing.T) {
	other := &metav1.OwnerReference{APIVersion: "orama.cloud/v1alpha1", Kind: "OramaCloudV1", Name: "v1", UID: "another-uid", Controller: boolPtr(true)}
	for name, disable := range map[string]func(*oramav1alpha1.DashboardSpec){
		"crons": func(d *oramav1alpha1.DashboardSpec) { d.Crons.Enabled = false; d.Crons.E2ECanary = nil },
		"dashboard": func(d *oramav1alpha1.DashboardSpec) {
			d.Enabled = false
			d.Crons.Enabled = false
			d.Crons.E2ECanary = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, cronOC(withCanary), dashEnvSecret(), canarySecret(),
				foreignCronJob("someone-elses", nil), foreignCronJob("same-labels-other-uid", other))
			h.mustRun()
			if got := len(h.cronJobs()); got != 6 {
				t.Fatalf("%d CronJobs before disabling, want 3 dashboard + canary + 2 foreign", got)
			}
			h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { disable(&o.Spec.Components.Dashboard) })
			h.mustRun()
			h.mustRun() // cleanup is idempotent
			if got, want := cronNames(h.cronJobs()), []string{"same-labels-other-uid", "someone-elses"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("CronJobs after disabling %s: %v, want %v", name, got, want)
			}
			if h.get("v1-e2e-canary-state", &corev1.PersistentVolumeClaim{}) {
				t.Error("the canary's state volume outlived the canary")
			}
		})
	}
}

func TestDashboardCronsCleanupDoesNotNeedTheEnvironment(t *testing.T) {
	h := newEngineHarness(t, cronOC(nil), dashEnvSecret())
	h.mustRun()
	if err := h.cl.Delete(context.Background(), dashEnvSecret()); err != nil {
		t.Fatal(err)
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Crons.Enabled = false })
	h.mustFail("dashboard.envFrom[0]: Secret orama-system/dash-env does not exist")
	if crons := h.cronJobs(); len(crons) != 0 {
		t.Fatalf("disabled crons still exist with the environment Secret missing: %v", cronNames(crons))
	}
}

func TestDashboardCronsBypassedAdmissionIsReportedAndCleanedUp(t *testing.T) {
	h := newEngineHarness(t, cronOC(nil), dashEnvSecret())
	h.mustRun()
	// The CRD refuses this; an object that bypassed admission still gets no crons.
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Enabled = false })
	h.mustFail("dashboard.crons requires the dashboard enabled")
	if crons := h.cronJobs(); len(crons) != 0 {
		t.Fatalf("CronJobs %v survived with the dashboard disabled", cronNames(crons))
	}
	if h.dashboard() != nil {
		t.Fatal("the dashboard Deployment survived its disable")
	}
	cond := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "ReconcileError" || !strings.HasPrefix(cond.Message, "dashboard: ") {
		t.Fatalf("Ready condition %+v", cond)
	}
}

func TestDashboardCronsNeverTouchAForeignSameNameCronJob(t *testing.T) {
	foreign := foreignCronJob("v1-dashboard-cron-crawler", nil)
	h := newEngineHarness(t, cronOC(nil), dashEnvSecret(), foreign)
	h.mustFail("CronJob orama-system/v1-dashboard-cron-crawler exists and is not controlled by OramaCloudV1 v1")
	got := h.cronJobs()["v1-dashboard-cron-crawler"]
	if got.Spec.Schedule != "5 5 * * *" || len(got.OwnerReferences) != 0 {
		t.Fatalf("the foreign CronJob was modified: %+v", got)
	}
}

func TestDashboardCronSecretPrecedence(t *testing.T) {
	sec := func(name, prefix string, data map[string]string) dashboardEnvSource {
		return dashboardEnvSource{Kind: "Secret", Name: name, Prefix: prefix, Data: data}
	}
	cm := func(name string, data map[string]string) dashboardEnvSource {
		return dashboardEnvSource{Kind: "ConfigMap", Name: name, Data: data}
	}
	has := map[string]string{"CRON_SECRET": "x"}
	cases := []struct {
		name    string
		env     []corev1.EnvVar
		sources []dashboardEnvSource
		want    *corev1.SecretKeySelector
		err     string
	}{
		{name: "the last Secret holding it wins", sources: []dashboardEnvSource{sec("a", "", has), sec("b", "", has)},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "b"}, Key: "CRON_SECRET"}},
		{name: "a later source without it does not win", sources: []dashboardEnvSource{sec("a", "", has), sec("b", "", map[string]string{"OTHER": "x"})},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}, Key: "CRON_SECRET"}},
		{name: "a prefix selects the underlying key", sources: []dashboardEnvSource{sec("a", "CRON_", map[string]string{"SECRET": "x", "CRON_SECRET": "y"})},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}, Key: "SECRET"}},
		{name: "a prefix that cannot produce the name is skipped", sources: []dashboardEnvSource{sec("a", "", has), sec("b", "X_", has)},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}, Key: "CRON_SECRET"}},
		{name: "an absent optional source is skipped", sources: []dashboardEnvSource{sec("a", "", has), sec("gone", "", nil)},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}, Key: "CRON_SECRET"}},
		{name: "a later ConfigMap overriding a Secret fails", sources: []dashboardEnvSource{sec("a", "", has), cm("c", has)},
			err: "the dashboard takes CRON_SECRET from ConfigMap c (dashboard.envFrom[1]); it must come from a Secret"},
		{name: "a Secret after a ConfigMap wins", sources: []dashboardEnvSource{cm("c", has), sec("a", "", has)},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "a"}, Key: "CRON_SECRET"}},
		{name: "an explicit env secretKeyRef wins over envFrom", env: []corev1.EnvVar{secretKeyEnv("CRON_SECRET", "explicit", "cron")},
			sources: []dashboardEnvSource{sec("a", "", has)},
			want:    &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "explicit"}, Key: "cron"}},
		{name: "the last env entry wins", env: []corev1.EnvVar{{Name: "CRON_SECRET", Value: "literal"}, secretKeyEnv("CRON_SECRET", "explicit", "cron")},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "explicit"}, Key: "cron"}},
		{name: "an optional env reference is required anyway", env: []corev1.EnvVar{{Name: "CRON_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "explicit"}, Key: "cron", Optional: optional()}}}},
			want: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "explicit"}, Key: "cron"}},
		{name: "an env literal fails", env: []corev1.EnvVar{{Name: "CRON_SECRET", Value: "literal"}}, sources: []dashboardEnvSource{sec("a", "", has)},
			err: "dashboard.env sets CRON_SECRET, but not from a Secret key"},
		{name: "an env ConfigMap key fails", env: []corev1.EnvVar{configMapKeyEnv("CRON_SECRET", "c", "cron")},
			err: "dashboard.env sets CRON_SECRET, but not from a Secret key"},
		{name: "no source fails", sources: []dashboardEnvSource{sec("a", "", map[string]string{"OTHER": "x"})},
			err: "dashboard.crons needs CRON_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := cronOC(func(d *oramav1alpha1.DashboardSpec) { d.Env = tc.env })
			got, err := dashboardCronSecret(oc, tc.sources)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestDashboardCronsConfigurationErrors(t *testing.T) {
	cases := []struct {
		name string
		oc   *oramav1alpha1.OramaCloudV1
		objs []client.Object
		err  string
	}{
		{name: "an optional source that is absent", oc: cronOC(func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "dash-env"}, Optional: optional()}}}
		}), err: "dashboard.crons needs CRON_SECRET"},
		{name: "an empty key", oc: cronOC(nil), objs: []client.Object{secretOf("dash-env", map[string]string{"CRON_SECRET": ""})},
			err: "dashboard.crons CRON_SECRET: Secret orama-system/dash-env has no non-empty key CRON_SECRET"},
		{name: "an explicit optional reference to a missing Secret", oc: cronOC(func(d *oramav1alpha1.DashboardSpec) {
			d.EnvFrom = nil
			d.Env = []corev1.EnvVar{{Name: "CRON_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "cron", Optional: optional()}}}}
		}), err: "dashboard.crons CRON_SECRET: Secret orama-system/missing does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, append([]client.Object{tc.oc}, tc.objs...)...)
			err := h.mustFail(tc.err)
			if strings.Contains(err.Error(), cronValue) || strings.Contains(err.Error(), canaryKey) {
				t.Fatalf("the error carries a secret value: %v", err)
			}
			if crons := h.cronJobs(); len(crons) != 0 {
				t.Fatalf("CronJobs %v created despite the error", cronNames(crons))
			}
			if cond := meta.FindStatusCondition(h.stored().Status.Conditions, "Ready"); cond == nil || cond.Reason != "ReconcileError" {
				t.Fatalf("Ready condition %+v", cond)
			}
		})
	}
}

func TestDashboardCronsRecoverWhenTheKeyReturns(t *testing.T) {
	h := newEngineHarness(t, cronOC(nil), secretOf("dash-env", map[string]string{"OTHER": "x"}))
	h.mustFail("dashboard.crons needs CRON_SECRET")
	h.setSecret("dash-env", "CRON_SECRET", cronValue)
	h.mustRun()
	want := []corev1.EnvVar{secretKeyEnv("CRON_SECRET", "dash-env", "CRON_SECRET")}
	before := h.cronJobs()
	if len(before) != 3 {
		t.Fatalf("CronJobs %v after the key returned", cronNames(before))
	}
	// A new value is read by the next Job through the same reference: nothing is rewritten.
	h.setSecret("dash-env", "CRON_SECRET", "cron-value-rotated")
	h.mustRun()
	for name, cj := range h.cronJobs() {
		if got := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: env %+v", name, got)
		}
		if cj.ResourceVersion != before[name].ResourceVersion {
			t.Errorf("%s rewritten by a value change", name)
		}
	}
}

func TestE2ECanaryCronJob(t *testing.T) {
	oc := cronOC(withCanary)
	oc.Spec.Images.PullSecretName = "regcred"
	h := newEngineHarness(t, oc, dashEnvSecret(), canarySecret())
	h.mustRun()

	cj, ok := h.cronJobs()["v1-e2e-canary"]
	if !ok {
		t.Fatal("CronJob v1-e2e-canary not created")
	}
	s, job := cj.Spec, cj.Spec.JobTemplate.Spec
	pod := job.Template.Spec
	if s.Schedule != "*/5 * * * *" || *s.TimeZone != "Etc/UTC" || !*s.Suspend || s.ConcurrencyPolicy != batchv1.ForbidConcurrent ||
		*s.StartingDeadlineSeconds != 60 || *s.SuccessfulJobsHistoryLimit != 1 || *s.FailedJobsHistoryLimit != 3 {
		t.Errorf("CronJob policy %+v", s)
	}
	if *job.BackoffLimit != 0 || *job.ActiveDeadlineSeconds != 900 || *job.TTLSecondsAfterFinished != 3600 {
		t.Errorf("Job policy %+v", job)
	}
	if !metav1.IsControlledBy(&cj, oc) || !reflect.DeepEqual(cj.Labels, labelsFor(oc, "e2e-canary")) {
		t.Errorf("owner %v labels %v", cj.OwnerReferences, cj.Labels)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken || *pod.SecurityContext.FSGroup != 65532 ||
		!reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "regcred"}}) {
		t.Errorf("pod %+v", pod)
	}
	if pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "v1-e2e-canary-state" ||
		pod.Volumes[1].EmptyDir == nil || pod.Volumes[1].EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("volumes %+v", pod.Volumes)
	}
	c := pod.Containers[0]
	if c.Image != canaryImage || c.Command[2] != e2eCanaryScript {
		t.Errorf("image %q", c.Image)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	for name, key := range map[string]string{"E2E_CANARY_COLLECTION_ID": "collectionID", "E2E_CANARY_INDEX_ID": "indexID", "E2E_CANARY_API_KEY": "apiKey"} {
		if want := secretKeyEnv(name, "canary", key); !reflect.DeepEqual(env[name], want) {
			t.Errorf("%s = %+v, want %+v", name, env[name], want)
		}
	}
	if env["E2E_CANARY_JWT_URL"].Value != "http://v1-dashboard:3000/api/user/jwt" || env["E2E_CANARY_READER_URL"].Value != "http://v1-reader" {
		t.Errorf("targets %v %v", env["E2E_CANARY_JWT_URL"], env["E2E_CANARY_READER_URL"])
	}
	if sc := c.SecurityContext; !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation {
		t.Errorf("securityContext %+v", sc)
	}
	if b, _ := json.Marshal(cj); strings.Contains(string(b), canaryKey) {
		t.Error("the canary's API key is in the CronJob")
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if !h.get("v1-e2e-canary-state", pvc) || !metav1.IsControlledBy(pvc, oc) ||
		pvc.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("1Gi")) != 0 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("state volume %+v", pvc)
	}
	// The canary follows crons.suspend, as the dashboard crons do.
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Crons.Suspend = boolPtr(false)
	})
	h.mustRun()
	for name, cj := range h.cronJobs() {
		if *cj.Spec.Suspend {
			t.Errorf("%s still suspended after crons.suspend=false", name)
		}
	}

	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Crons.E2ECanary = nil })
	h.mustRun()
	if _, ok := h.cronJobs()["v1-e2e-canary"]; ok || h.get("v1-e2e-canary-state", &corev1.PersistentVolumeClaim{}) {
		t.Error("removing e2eCanary left its CronJob or state volume")
	}
}

// A missing or partial canary Secret must not keep the dashboard crons from being written: they are
// written first, and the reconcile then fails on the canary alone.
func TestE2ECanarySecretErrorsLeaveTheDashboardCrons(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"missing": {dashEnvSecret()},
		"partial": {dashEnvSecret(), secretOf("canary", map[string]string{"collectionID": "col-1", "indexID": "idx-1"})},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t, append([]client.Object{cronOC(withCanary)}, objs...)...)
			h.mustFail("dashboard.crons.e2eCanary.secretRef: Secret orama-system/canary")
			if got := cronNames(h.cronJobs()); !reflect.DeepEqual(got, dashboardCronNames) {
				t.Fatalf("CronJobs %v, want the dashboard's three", got)
			}
			if h.get("v1-e2e-canary-state", &corev1.PersistentVolumeClaim{}) {
				t.Fatal("state volume created before the canary Secret was valid")
			}
			if name == "missing" {
				if err := h.cl.Create(context.Background(), canarySecret()); err != nil {
					t.Fatal(err)
				}
			} else {
				h.setSecret("canary", "apiKey", canaryKey)
			}
			h.mustRun()
			if _, ok := h.cronJobs()["v1-e2e-canary"]; !ok {
				t.Fatal("the canary was not created once its Secret was complete")
			}
		})
	}
}

func TestE2ECanaryNeverAdoptsAForeignStateVolume(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "v1-e2e-canary-state", Namespace: "orama-system"}}
	h := newEngineHarness(t, cronOC(withCanary), dashEnvSecret(), canarySecret(), foreign)
	h.mustFail("PersistentVolumeClaim orama-system/v1-e2e-canary-state exists and is not controlled by OramaCloudV1 v1")
	got := &corev1.PersistentVolumeClaim{}
	h.get("v1-e2e-canary-state", got)
	if len(got.OwnerReferences) != 0 || len(got.Labels) != 0 {
		t.Fatalf("the foreign claim was modified: %+v", got.ObjectMeta)
	}
}

// TestDashboardCronsAdmission checks the generated CRD's rules on a real API server.
func TestDashboardCronsAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the OramaCloudV1 admission envtest")
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cl, err := client.New(cfg, client.Options{Scheme: ocv1TestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func(*oramav1alpha1.DashboardSpec)
		err  string
	}{
		{name: "crons with the dashboard", mut: func(d *oramav1alpha1.DashboardSpec) { d.Crons.Enabled = true }},
		{name: "crons without the dashboard", mut: func(d *oramav1alpha1.DashboardSpec) { d.Enabled = false; d.Crons.Enabled = true },
			err: "dashboard.crons requires the dashboard enabled"},
		{name: "both off", mut: func(d *oramav1alpha1.DashboardSpec) { d.Enabled = false }},
		{name: "a pinned canary", mut: func(d *oramav1alpha1.DashboardSpec) { d.Crons.Enabled = true; withCanary(d) }},
		{name: "a canary without crons", mut: withCanary, err: "dashboard.crons.e2eCanary requires dashboard.crons enabled"},
		{name: "a canary without an image", mut: func(d *oramav1alpha1.DashboardSpec) {
			d.Crons.Enabled = true
			withCanary(d)
			d.Crons.E2ECanary.Image = ""
		}, err: "spec.components.dashboard.crons.e2eCanary.image"},
		{name: "a canary on a tag", mut: func(d *oramav1alpha1.DashboardSpec) {
			d.Crons.Enabled = true
			withCanary(d)
			d.Crons.E2ECanary.Image = "oramasearch/oramacore-test-e2e:latest"
		}, err: "spec.components.dashboard.crons.e2eCanary.image"},
		{name: "a canary without a Secret name", mut: func(d *oramav1alpha1.DashboardSpec) {
			d.Crons.Enabled = true
			withCanary(d)
			d.Crons.E2ECanary.SecretRef.Name = ""
		}, err: "dashboard.crons.e2eCanary.secretRef needs a name"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := dashOC(tc.mut)
			oc.Name, oc.Namespace, oc.UID = "v1-"+strconv.Itoa(i), "default", ""
			oc.Spec.IDP.Provider = "none" // a required enum the typed client would send empty
			err := cl.Create(context.Background(), oc)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.err != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("error = %v, want an Invalid error containing %q", err, tc.err)
			case tc.err == "" && tc.mut != nil && oc.Spec.Components.Dashboard.Crons.Enabled:
				if s := oc.Spec.Components.Dashboard.Crons.Suspend; s == nil || !*s {
					t.Fatalf("crons.suspend defaulted to %v, want true", s)
				}
			}
		})
	}
}

// runShell runs script with sh, adding env, and returns the combined output and exit code.
func runShell(t *testing.T, shell, script string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(shell, "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// shells are the POSIX shells on this machine to run the rendered scripts with.
func shells(t *testing.T, tools ...string) []string {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required to run the rendered script", tool)
		}
	}
	var out []string
	for _, sh := range []string{"sh", "dash"} {
		if _, err := exec.LookPath(sh); err == nil {
			out = append(out, sh)
		}
	}
	return out
}

func TestDashboardCronScriptAgainstAServer(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	redirectTargetHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/elsewhere" {
			redirectTargetHit = true
			return
		}
		auth = append(auth, r.Header.Get("Authorization"))
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if code == http.StatusFound {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "tenant-body-0003")
	}))
	defer srv.Close()
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + refused.Addr().String() + "/status/200"
	_ = refused.Close()

	for _, sh := range shells(t, "curl") {
		for _, tc := range []struct {
			url     string
			ok      bool
			logline string
		}{
			{srv.URL + "/status/200", true, "HTTP 200"},
			{srv.URL + "/status/204", true, "HTTP 204"},
			{srv.URL + "/status/302", false, "HTTP 302"},
			{srv.URL + "/status/401", false, "HTTP 401"},
			{srv.URL + "/status/500", false, "HTTP 500"},
			{refusedURL, false, "HTTP 000"},
		} {
			t.Run(sh+" "+tc.logline, func(t *testing.T) {
				auth = nil
				out, code := runShell(t, sh, dashboardCronScript(tc.url), "CRON_SECRET="+cronValue)
				if (code == 0) != tc.ok || !strings.Contains(out, tc.logline) {
					t.Fatalf("exit %d, output %q", code, out)
				}
				if strings.Contains(out, cronValue) || strings.Contains(out, "tenant-body") {
					t.Fatalf("the output carries the bearer or the body: %q", out)
				}
				if tc.url != refusedURL && !reflect.DeepEqual(auth, []string{"Bearer " + cronValue}) {
					t.Fatalf("Authorization headers %q", auth)
				}
			})
		}
		t.Run(sh+" empty secret", func(t *testing.T) {
			auth = nil
			if out, code := runShell(t, sh, dashboardCronScript(srv.URL+"/status/200"), "CRON_SECRET="); code == 0 || len(auth) != 0 {
				t.Fatalf("exit %d with no secret, %d calls, output %q", code, len(auth), out)
			}
		})
	}
	if redirectTargetHit {
		t.Fatal("a redirect was followed")
	}
}

// canaryServer fakes the dashboard's /api/user/jwt and the engine routes the prune uses.
type canaryServer struct {
	*httptest.Server
	mu      sync.Mutex
	count   int
	titles  []string
	jwtBody map[string]string
	deleted []string
	auth    []string
}

func newCanaryServer(t *testing.T, count int, titles ...string) *canaryServer {
	s := &canaryServer{count: count, titles: titles}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/api/user/jwt":
			_ = json.NewDecoder(r.Body).Decode(&s.jwtBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"jwt": "jwt-value-0004", "readerApiKey": "rkey", "readerURL": s.URL, "writerURL": s.URL})
		case r.URL.Path == "/v1/collections/col-1/stats" && r.URL.Query().Get("api-key") == "rkey":
			_ = json.NewEncoder(w).Encode(map[string]any{"document_count": s.count - len(s.deleted), "fields": map[string]any{"title": s.titles}})
		case r.URL.Path == "/v1/collections/col-1/search":
			var q struct {
				Where struct{ Title string } `json:"where"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]string{{"id": "id-" + strings.TrimPrefix(q.Where.Title, "monitor ")}}})
		case r.URL.Path == "/v1/collections/col-1/indexes/idx-1/delete":
			s.auth = append(s.auth, r.Header.Get("Authorization"))
			var ids []string
			_ = json.NewDecoder(r.Body).Decode(&ids)
			s.deleted = append(s.deleted, ids...)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// runCanary runs the rendered canary script with a fake monitor-test that records its config and
// tracks tracked titles, exiting canaryExit. lastPrune is written to prune-last-run first when set.
func runCanary(t *testing.T, sh string, srv *canaryServer, canaryExit int, tracked []string, lastPrune string) (state, out string, code int) {
	t.Helper()
	dir := t.TempDir()
	state, run, bin := filepath.Join(dir, "state"), filepath.Join(dir, "run"), filepath.Join(dir, "bin")
	for _, d := range []string{state, run, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	trackedJSON, _ := json.Marshal(tracked)
	fake := "#!/bin/sh\ncp \"$CONFIG_PATH\" ../config-seen.json || exit 3\n" +
		"printf '{\"counter\":1,\"documentTitles\":%s,\"lastRun\":\"x\"}' '" + string(trackedJSON) + "' > test-state.json\n" +
		"exit " + strconv.Itoa(canaryExit) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "monitor-test"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if lastPrune != "" {
		if err := os.WriteFile(filepath.Join(state, "prune-last-run"), []byte(lastPrune), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, code = runShell(t, sh, e2eCanaryScript,
		"PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+dir,
		"E2E_CANARY_COLLECTION_ID=col-1", "E2E_CANARY_INDEX_ID=idx-1", "E2E_CANARY_API_KEY="+canaryKey,
		"E2E_CANARY_JWT_URL="+srv.URL+"/api/user/jwt", "E2E_CANARY_READER_URL=http://v1-reader",
		"E2E_CANARY_STATE_DIR="+state, "E2E_CANARY_RUN_DIR="+run)
	if strings.Contains(out, canaryKey) || strings.Contains(out, "jwt-value-0004") {
		t.Fatalf("the output carries a secret: %q", out)
	}
	return state, out, code
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestE2ECanaryScript(t *testing.T) {
	titles := []string{"monitor aaaaaaaa", "monitor bbbbbbbb", "monitor cccccccc"}
	for _, sh := range shells(t, "curl", "jq") {
		t.Run(sh+" prunes leaked probes once a day", func(t *testing.T) {
			srv := newCanaryServer(t, 300, titles...)
			state, out, code := runCanary(t, sh, srv, 0, []string{"monitor aaaaaaaa"}, "")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if !reflect.DeepEqual(srv.deleted, []string{"id-bbbbbbbb", "id-cccccccc"}) || !reflect.DeepEqual(srv.auth, []string{"Bearer jwt-value-0004"}) {
				t.Fatalf("deleted %v with %q", srv.deleted, srv.auth)
			}
			if want := map[string]string{"collectionId": "col-1", "privateApiKey": canaryKey, "scope": "write"}; !reflect.DeepEqual(srv.jwtBody, want) {
				t.Fatalf("JWT request %v", srv.jwtBody)
			}
			var cfg map[string]any
			if err := json.Unmarshal([]byte(readFile(t, filepath.Join(state, "..", "config-seen.json"))), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg["authJwtURL"] != srv.URL+"/api/user/jwt" || cfg["apiKey"] != canaryKey || cfg["collectionID"] != "col-1" ||
				cfg["metricsFilePath"] != filepath.Join(state, "metrics", "oramacore-test-e2e.prom") {
				t.Fatalf("canary config %v", cfg)
			}
			metrics := readFile(t, filepath.Join(state, "metrics", "oramacore-collection-prune.prom"))
			for _, line := range []string{"oramacore_collection_prune_success 1", "oramacore_collection_prune_last_deleted 2", `oramacore_collection_prune_last_count{collection_id="col-1"} 300`} {
				if !strings.Contains(metrics, line) {
					t.Errorf("metrics lack %q:\n%s", line, metrics)
				}
			}
			last, _ := strconv.ParseInt(strings.TrimSpace(readFile(t, filepath.Join(state, "prune-last-run"))), 10, 64)
			if time.Since(time.Unix(last, 0)) > time.Minute {
				t.Errorf("prune-last-run %d", last)
			}
		})
		t.Run(sh+" skips the prune until the next 04:30 UTC", func(t *testing.T) {
			srv := newCanaryServer(t, 300, titles...)
			if _, out, code := runCanary(t, sh, srv, 0, []string{"monitor aaaaaaaa"}, strconv.FormatInt(time.Now().Unix(), 10)); code != 0 || srv.jwtBody != nil {
				t.Fatalf("exit %d, JWT request %v: %s", code, srv.jwtBody, out)
			}
		})
		t.Run(sh+" prunes even when the canary fails, and fails the run", func(t *testing.T) {
			srv := newCanaryServer(t, 300, titles...)
			if _, out, code := runCanary(t, sh, srv, 1, []string{"monitor aaaaaaaa"}, ""); code == 0 || len(srv.deleted) != 2 {
				t.Fatalf("exit %d, deleted %v: %s", code, srv.deleted, out)
			}
		})
		t.Run(sh+" leaves a collection under the threshold alone", func(t *testing.T) {
			srv := newCanaryServer(t, 150, titles...)
			if _, out, code := runCanary(t, sh, srv, 0, []string{"monitor aaaaaaaa"}, ""); code != 0 || len(srv.deleted) != 0 {
				t.Fatalf("exit %d, deleted %v: %s", code, srv.deleted, out)
			}
		})
		t.Run(sh+" refuses an empty keep-list", func(t *testing.T) {
			srv := newCanaryServer(t, 300, titles...)
			state, out, code := runCanary(t, sh, srv, 0, []string{}, "")
			if code == 0 || len(srv.deleted) != 0 || !strings.Contains(out, "refusing to prune") {
				t.Fatalf("exit %d, deleted %v: %s", code, srv.deleted, out)
			}
			if m := readFile(t, filepath.Join(state, "metrics", "oramacore-collection-prune.prom")); !strings.Contains(m, "oramacore_collection_prune_success 0") {
				t.Fatalf("metrics:\n%s", m)
			}
		})
	}
}
