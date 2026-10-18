/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// importOC is a stack whose processor imports from the readers: the engine, the in-cluster
// ClickHouse and control DB, and the processor with import.enabled.
func importOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return stackOC(func(o *oramav1alpha1.OramaCloudV1) {
		c := &o.Spec.Components
		c.Analytics = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		c.ControlDB = oramav1alpha1.DataStoreSpec{Enabled: true, Mode: "in-cluster"}
		c.AnalyticsProcessor.Import.Enabled = true
		if mut != nil {
			mut(o)
		}
	})
}

// newImportHarness reconciles oc once with the engine's Secret in place.
func newImportHarness(t *testing.T, oc *oramav1alpha1.OramaCloudV1, objs ...client.Object) *engineHarness {
	t.Helper()
	h := newEngineHarness(t, append([]client.Object{oc, engineSecret()}, objs...)...)
	h.mustRun()
	return h
}

// cronJob reads <cr>-analytics-import and reports whether it exists.
func (h *engineHarness) cronJob() (*batchv1.CronJob, bool) {
	h.t.Helper()
	cron := &batchv1.CronJob{}
	return cron, h.get("v1-analytics-import", cron)
}

// processorSources is the source names in the processor's ConfigMap.
func (h *engineHarness) processorSources() []string {
	h.t.Helper()
	cm := &corev1.ConfigMap{}
	if !h.get("v1-analytics-processor-config", cm) {
		h.t.Fatal("ConfigMap v1-analytics-processor-config not found")
	}
	var names []string
	for _, src := range parseYAML(h.t, cm.Data["config.yaml"])["oramacores"].([]any) {
		names = append(names, src.(map[string]any)["name"].(string))
	}
	return names
}

func TestReaderStatefulSetIsGovernedByHeadlessService(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("import enabled %v", enabled), func(t *testing.T) {
			h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.AnalyticsProcessor.Import.Enabled = enabled
			}))
			if got := h.reader().Spec.ServiceName; got != "v1-reader-headless" {
				t.Errorf("reader serviceName = %q, want v1-reader-headless", got)
			}
			svc := h.service("v1-reader-headless")
			wantPorts := []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt32(8080)}}
			if svc.Spec.ClusterIP != corev1.ClusterIPNone || !reflect.DeepEqual(svc.Spec.Ports, wantPorts) ||
				!reflect.DeepEqual(svc.Spec.Selector, labelsFor(h.stored(), "reader")) {
				t.Errorf("headless Service spec = %+v", svc.Spec)
			}
		})
	}
}

func TestAnalyticsImportSourcesListEachReaderOnce(t *testing.T) {
	imports := map[string]oramav1alpha1.AnalyticsImportSpec{
		"enabled": {Enabled: true}, "disabled": {}, "suspended": {Enabled: true, Suspend: true},
	}
	for _, replicas := range []int32{0, 1, 2, 3} {
		want := int(replicas)
		if want == 0 {
			want = 3
		}
		for name, imp := range imports {
			t.Run(fmt.Sprintf("replicas %d, import %s", replicas, name), func(t *testing.T) {
				cfg := analyticsProcessorConfigYAML(importOC(func(o *oramav1alpha1.OramaCloudV1) {
					o.Spec.Components.Engine.Reader.Replicas = replicas
					o.Spec.Components.AnalyticsProcessor.Import = imp
				}))
				sources := parseYAML(t, cfg)["oramacores"].([]any)
				if len(sources) != want {
					t.Fatalf("%d sources, want %d:\n%s", len(sources), want, cfg)
				}
				for n, src := range sources {
					reader := fmt.Sprintf("v1-reader-%d", n)
					wantSrc := map[string]any{"name": reader, "enabled": true, "api_key": "__ANALYTICS_API_KEY__",
						"api_url": "http://" + reader + ".v1-reader-headless.orama-system.svc:8080/v1/analytics"}
					if !reflect.DeepEqual(src, wantSrc) {
						t.Errorf("source %d = %v, want %v", n, src, wantSrc)
					}
				}
				for _, s := range []string{"-writer", "__MASTER_API_KEY__", "http://v1-reader/"} {
					if strings.Contains(cfg, s) {
						t.Errorf("the processor config names %q", s)
					}
				}
			})
		}
	}
	// The issue's worked example: v1-dev in namespace v1-dev with two readers.
	t.Run("v1-dev with two readers", func(t *testing.T) {
		oc := importOC(func(o *oramav1alpha1.OramaCloudV1) {
			o.Name, o.Namespace = "v1-dev", "v1-dev"
			o.Spec.Components.Engine.Reader.Replicas = 2
		})
		want := parseYAML(t, `oramacores:
  - name: v1-dev-reader-0
    api_url: http://v1-dev-reader-0.v1-dev-reader-headless.v1-dev.svc:8080/v1/analytics
    api_key: __ANALYTICS_API_KEY__
    enabled: true
  - name: v1-dev-reader-1
    api_url: http://v1-dev-reader-1.v1-dev-reader-headless.v1-dev.svc:8080/v1/analytics
    api_key: __ANALYTICS_API_KEY__
    enabled: true
`)["oramacores"]
		if got := parseYAML(t, analyticsProcessorConfigYAML(oc))["oramacores"]; !reflect.DeepEqual(got, want) {
			t.Errorf("oramacores = %v, want %v", got, want)
		}
	})
}

func TestAnalyticsImportCronJobIsHardened(t *testing.T) {
	oc := importOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.PullSecretName = "private-images" })
	h := newImportHarness(t, oc)
	cron, ok := h.cronJob()
	if !ok {
		t.Fatal("CronJob v1-analytics-import not found")
	}
	stored := h.stored()
	s := cron.Spec
	if s.Schedule != "*/5 * * * *" || s.TimeZone == nil || *s.TimeZone != "Etc/UTC" ||
		s.ConcurrencyPolicy != batchv1.ForbidConcurrent || s.Suspend == nil || *s.Suspend ||
		*s.StartingDeadlineSeconds != 60 || *s.SuccessfulJobsHistoryLimit != 1 || *s.FailedJobsHistoryLimit != 1 {
		t.Errorf("CronJob spec = %+v", s)
	}
	job := s.JobTemplate.Spec
	if *job.BackoffLimit != 0 || *job.ActiveDeadlineSeconds != 300 || *job.TTLSecondsAfterFinished != 3600 {
		t.Errorf("Job template = %+v", job)
	}
	lbls := labelsFor(stored, "analytics-import")
	if !reflect.DeepEqual(cron.Labels, lbls) || !reflect.DeepEqual(job.Template.Labels, lbls) || !metav1.IsControlledBy(cron, stored) {
		t.Errorf("labels %v / %v, controlled by the CR %v", cron.Labels, job.Template.Labels, metav1.IsControlledBy(cron, stored))
	}
	pod := job.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		!reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "private-images"}}) ||
		len(pod.Volumes) != 0 || len(pod.InitContainers) != 0 || len(pod.Containers) != 1 {
		t.Fatalf("pod spec = %+v", pod)
	}
	c := pod.Containers[0]
	if c.Name != "import" || c.Image != "curlimages/curl:8.10.1" || c.ImagePullPolicy != corev1.PullIfNotPresent ||
		!reflect.DeepEqual(c.Command, []string{"sh", "-c", analyticsImportScript(stored)}) || len(c.Args) != 0 {
		t.Errorf("import container = %+v", c)
	}
	if strings.Contains(analyticsImportScript(stored), "--retry") {
		t.Error("the import retries its requests")
	}
	if len(c.Env) != 0 || len(c.EnvFrom) != 0 || len(c.VolumeMounts) != 0 {
		t.Errorf("the import container reads credentials: env %v, envFrom %v, mounts %v", c.Env, c.EnvFrom, c.VolumeMounts)
	}
	if !reflect.DeepEqual(c.SecurityContext, hardenedSecurityContext()) || !reflect.DeepEqual(c.Resources, *resourceReqs("32Mi", "10m", "64Mi")) ||
		c.TerminationMessagePath != corev1.TerminationMessagePathDefault || c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("import container hardening = %+v", c)
	}
}

// spec.images.overrides["analytics-import"] replaces the import's curl image, so a set can pin it.
func TestAnalyticsImportImageOverride(t *testing.T) {
	const pinned = "oramasearch/curl@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.Overrides = map[string]string{"analytics-import": pinned}
	}))
	cron, ok := h.cronJob()
	if !ok {
		t.Fatal("CronJob v1-analytics-import not found")
	}
	if got := cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image; got != pinned {
		t.Errorf("import image = %q, want %q", got, pinned)
	}
}

// The Job imports, then aggregates (Prod 1's aggregator timer), with the same curl hardening on both
// calls; the aggregation runs even when the import fails, and the Job fails when either call does.
// The script runs under sh with a curl stub that records its arguments.
func TestAnalyticsImportScriptImportsThenAggregates(t *testing.T) {
	dir := t.TempDir()
	stub := `#!/bin/sh
for a in "$@"; do last=$a; done
printf '%s\n' "$*" >> "$CALLS"
case "$last" in */import) exit "$IMPORT_RC" ;; */aggregate2) exit "$AGGREGATE_RC" ;; esac
exit 99
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	flags := "--fail --silent --show-error --connect-timeout 10 --request POST --write-out \\n"
	wantCalls := []string{
		flags + " --max-time 180 http://v1-analytics-processor:8080/import",
		flags + " --max-time 110 http://v1-analytics-processor:8080/aggregate2",
	}
	script := analyticsImportScript(importOC(nil))
	for _, tc := range []struct {
		importRC, aggregateRC string
		wantFail              bool
	}{{"0", "0", false}, {"22", "0", true}, {"0", "22", true}, {"7", "28", true}} {
		calls := filepath.Join(dir, "calls-"+tc.importRC+"-"+tc.aggregateRC)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls,
			"IMPORT_RC="+tc.importRC, "AGGREGATE_RC="+tc.aggregateRC)
		err := cmd.Run()
		if (err != nil) != tc.wantFail {
			t.Errorf("import %s, aggregate %s: error %v, want failure %v", tc.importRC, tc.aggregateRC, err, tc.wantFail)
		}
		got, readErr := os.ReadFile(calls)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if lines := strings.Split(strings.TrimSpace(string(got)), "\n"); !reflect.DeepEqual(lines, wantCalls) {
			t.Errorf("import %s, aggregate %s: calls %q, want %q", tc.importRC, tc.aggregateRC, lines, wantCalls)
		}
	}
}

// With the API's defaults filled in, an unchanged reconcile writes neither the CronJob nor the
// processor; changing only import.suspend updates the CronJob in place and keeps the processor's
// config hash.
func TestAnalyticsImportSteadyReconcileWritesNothing(t *testing.T) {
	h := newImportHarness(t, importOC(nil))
	cron, _ := h.cronJob()
	pod := &cron.Spec.JobTemplate.Spec.Template.Spec
	pod.DNSPolicy, pod.SchedulerName = corev1.DNSClusterFirst, corev1.DefaultSchedulerName
	pod.TerminationGracePeriodSeconds, pod.SecurityContext = int64Ptr(30), &corev1.PodSecurityContext{}
	cron.Spec.JobTemplate.Spec.Completions, cron.Spec.JobTemplate.Spec.Parallelism = int32Ptr(1), int32Ptr(1)
	if err := h.cl.Update(context.Background(), cron); err != nil {
		t.Fatal(err)
	}
	h.updates = map[string]int{}
	before := h.templates()["analytics-processor"].Annotations[configHashKey]
	h.mustRun()
	for _, name := range []string{"v1-analytics-import", "v1-analytics-processor", "v1-analytics-processor-config"} {
		if h.updates[name] != 0 {
			t.Errorf("an unchanged reconcile updated %s %d times", name, h.updates[name])
		}
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Import.Suspend = true })
	h.updates = map[string]int{}
	h.mustRun()
	after, _ := h.cronJob()
	if after.UID != cron.UID || !*after.Spec.Suspend || h.updates["v1-analytics-import"] != 1 {
		t.Errorf("suspend: UID %s -> %s, suspend %v, %d updates", cron.UID, after.UID, *after.Spec.Suspend, h.updates["v1-analytics-import"])
	}
	if h.updates["v1-analytics-processor"] != 0 || h.templates()["analytics-processor"].Annotations[configHashKey] != before {
		t.Error("changing import.suspend changed the processor")
	}
}

func TestAnalyticsImportSuspend(t *testing.T) {
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.Replicas = 2
		o.Spec.Components.AnalyticsProcessor.Import.Suspend = true
	}))
	cron, ok := h.cronJob()
	if !ok || cron.Spec.Suspend == nil || !*cron.Spec.Suspend {
		t.Fatalf("suspended import: CronJob %v, spec.suspend %v", ok, cron.Spec.Suspend)
	}
	if h.service("v1-reader-headless").Spec.ClusterIP != corev1.ClusterIPNone {
		t.Error("the headless Service is not headless")
	}
	if got := h.processorSources(); !reflect.DeepEqual(got, []string{"v1-reader-0", "v1-reader-1"}) {
		t.Errorf("suspended sources = %v", got)
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Import.Suspend = false })
	h.mustRun()
	after, _ := h.cronJob()
	if after.UID != cron.UID || after.Spec.Suspend == nil || *after.Spec.Suspend {
		t.Errorf("resumed: UID %s -> %s, spec.suspend %v", cron.UID, after.UID, after.Spec.Suspend)
	}
}

func TestAnalyticsImportRequiresEngineAnalyticsAndControlDB(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*oramav1alpha1.OramaCloudV1)
		want string
	}{
		"engine disabled":    {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false }, "requires engine enabled"},
		"analytics disabled": {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Analytics.Enabled = false }, "requires analytics (the in-cluster ClickHouse) enabled"},
		"analytics external": {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Analytics.Mode = "external" }, "requires analytics (the in-cluster ClickHouse) enabled"},
		"controlDB disabled": {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = false }, "requires controlDB enabled"},
		"negative replicas":  {func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = -1 }, "engine.reader.replicas is -1, an invalid reader count"},
		"earlier step failing": {func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.ControlDB.Enabled = false
			o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage} // fails validateEngineImages
		}, "requires controlDB enabled"},
	} {
		for _, suspend := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, suspend %v", name, suspend), func(t *testing.T) {
				h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) {
					o.Spec.Components.AnalyticsProcessor.Import.Suspend = suspend
				}))
				if _, ok := h.cronJob(); !ok {
					t.Fatal("no CronJob to remove")
				}
				h.updateCR(tc.mut)
				if err := h.run(); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "v1-analytics-import is removed") {
					t.Errorf("Reconcile error = %v, want %q", err, tc.want)
				}
				if _, ok := h.cronJob(); ok {
					t.Error("the CronJob survived an invalid import")
				}
				if ready := h.readyCondition(); ready == nil || !strings.Contains(ready.Message, tc.want) {
					t.Errorf("Ready = %+v", ready)
				}
			})
		}
	}
	// A disabled import loses its schedule even when an earlier component step fails.
	t.Run("disabled import, earlier step failing", func(t *testing.T) {
		h := newImportHarness(t, importOC(nil))
		h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.AnalyticsProcessor.Import.Enabled = false
			o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage}
		})
		if err := h.run(); err == nil || !strings.Contains(err.Error(), "oramacore-writer and oramacore-reader") {
			t.Errorf("Reconcile error = %v, want the engine image error", err)
		}
		if _, ok := h.cronJob(); ok {
			t.Error("the CronJob survived a disabled import")
		}
	})
	// reconcileAnalyticsProcessor checks the same for a direct caller.
	t.Run("direct call", func(t *testing.T) {
		h := newImportHarness(t, importOC(nil))
		oc := h.stored()
		oc.Spec.Components.ControlDB.Enabled = false
		if err := h.r.reconcileAnalyticsProcessor(context.Background(), oc); err == nil || !strings.Contains(err.Error(), "requires controlDB enabled") {
			t.Errorf("reconcileAnalyticsProcessor error = %v", err)
		}
		if _, ok := h.cronJob(); ok {
			t.Error("the CronJob survived an invalid import")
		}
	})
}

func TestDisablingImportKeepsHeadlessService(t *testing.T) {
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = 2 }))
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Import.Enabled = false })
	h.mustRun()
	if _, ok := h.cronJob(); ok {
		t.Error("disabling the import kept its CronJob")
	}
	if h.service("v1-reader-headless").Spec.ClusterIP != corev1.ClusterIPNone || h.reader().Spec.ServiceName != "v1-reader-headless" {
		t.Error("disabling the import changed the readers' governing Service")
	}
	if got := h.processorSources(); !reflect.DeepEqual(got, []string{"v1-reader-0", "v1-reader-1"}) {
		t.Errorf("sources after disabling the import = %v", got)
	}
}

// Disabling the processor removes its ConfigMap (an older operator's unlabeled one included), its
// workload, the retention Job and the import CronJob; enabling it again brings them all back.
func TestDisablingProcessorRemovesImportAndConfig(t *testing.T) {
	h := newImportHarness(t, importOC(nil))
	cm := &corev1.ConfigMap{}
	h.get("v1-analytics-processor-config", cm)
	cm.Labels = nil
	if err := h.cl.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	gone := map[string]client.Object{
		"v1-analytics-processor-config": &corev1.ConfigMap{}, "v1-analytics-processor": &appsv1.Deployment{},
		"v1-analytics-import": &batchv1.CronJob{}, "v1-analytics-retention": &batchv1.Job{},
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Enabled = false })
	h.mustRun()
	for name, obj := range gone {
		if h.get(name, obj) {
			t.Errorf("%s survived disabling the processor", name)
		}
	}
	if h.get("v1-analytics-processor", &corev1.Service{}) {
		t.Error("the processor Service survived disabling the processor")
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Enabled = true })
	h.mustRun()
	for name, obj := range gone {
		if !h.get(name, obj) {
			t.Errorf("%s was not recreated", name)
		}
	}
	h.get("v1-analytics-processor-config", cm)
	if !reflect.DeepEqual(cm.Labels, labelsFor(h.stored(), "analytics-processor")) {
		t.Errorf("recreated ConfigMap labels = %v", cm.Labels)
	}
}

func TestDisablingProcessorLeavesForeignConfigMap(t *testing.T) {
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "v1-analytics-processor-config", Namespace: "orama-system"},
		Data: map[string]string{"owner": "someone else"}}
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.AnalyticsProcessor.Enabled = false }), foreign)
	cm := &corev1.ConfigMap{}
	if !h.get(foreign.Name, cm) || !reflect.DeepEqual(cm.Data, foreign.Data) {
		t.Errorf("foreign ConfigMap = %+v", cm)
	}
}

// failingDeletes fails every Delete of the named object.
type failingDeletes struct {
	client.Client
	name string
}

func (f failingDeletes) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetName() == f.name {
		return errors.New("injected delete failure")
	}
	return f.Client.Delete(ctx, obj, opts...)
}

func TestDisablingProcessorReturnsCleanupErrors(t *testing.T) {
	for _, labelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("labelled %v", labelled), func(t *testing.T) {
			h := newImportHarness(t, importOC(nil))
			if !labelled {
				cm := &corev1.ConfigMap{}
				h.get("v1-analytics-processor-config", cm)
				cm.Labels = nil
				if err := h.cl.Update(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			}
			h.r.Client = failingDeletes{Client: h.cl, name: "v1-analytics-processor-config"}
			oc := h.stored()
			oc.Spec.Components.AnalyticsProcessor.Enabled = false
			if err := h.r.reconcileAnalyticsProcessor(context.Background(), oc); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
				t.Errorf("reconcileAnalyticsProcessor error = %v, want the delete failure", err)
			}
		})
	}
}

// The processor's pod carries the hash of its config template: a changed reader count changes the
// sources and the hash, an unchanged one keeps both.
func TestProcessorConfigHashFollowsReaderSources(t *testing.T) {
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = 2 }))
	cm := &corev1.ConfigMap{}
	h.get("v1-analytics-processor-config", cm)
	hash := h.templates()["analytics-processor"].Annotations[configHashKey]
	if hash != templateSHA256(cm.Data["config.yaml"]) {
		t.Errorf("config hash %q is not the template's SHA-256", hash)
	}
	h.mustRun()
	if got := h.templates()["analytics-processor"].Annotations[configHashKey]; got != hash {
		t.Errorf("an unchanged config changed the hash: %q -> %q", hash, got)
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Reader.Replicas = 3 })
	h.mustRun()
	if got := h.processorSources(); !reflect.DeepEqual(got, []string{"v1-reader-0", "v1-reader-1", "v1-reader-2"}) {
		t.Errorf("sources after scaling to 3 = %v", got)
	}
	if h.templates()["analytics-processor"].Annotations[configHashKey] == hash {
		t.Error("a changed reader count kept the processor's config hash")
	}
}

func TestE2EScriptDoesNotDrainAnalytics(t *testing.T) {
	if strings.Contains(e2eFunctionalScript, "/v1/analytics") {
		t.Error("the functional e2e script calls the analytics export, which erases the readers' events")
	}
}

func TestAnalyticsRetentionJobKeepsThirtyDays(t *testing.T) {
	h := newImportHarness(t, importOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.PullSecretName = "private-images" }))
	job := &batchv1.Job{}
	if !h.get("v1-analytics-retention", job) {
		t.Fatal("Job v1-analytics-retention not found")
	}
	oc := h.stored()
	script := analyticsRetentionScript(oc)
	if !metav1.IsControlledBy(job, oc) || !reflect.DeepEqual(job.Labels, labelsFor(oc, "analytics-retention")) ||
		job.Annotations[analyticsRetentionHashKey] != templateSHA256(script) {
		t.Errorf("Job metadata = %+v", job.ObjectMeta)
	}
	pod := job.Spec.Template.Spec
	if *job.Spec.BackoffLimit != 10 || job.Spec.TTLSecondsAfterFinished != nil || pod.RestartPolicy != corev1.RestartPolicyNever ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		!reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "private-images"}}) || len(pod.Containers) != 1 {
		t.Fatalf("Job spec = %+v", job.Spec)
	}
	c := pod.Containers[0]
	wantEnv := []corev1.EnvVar{secretKeyEnv("CLICKHOUSE_PASSWORD", "v1-analytics-secret", "CLICKHOUSE_PASSWORD")}
	if c.Image != clickhouseImage || !reflect.DeepEqual(c.Command, []string{"sh", "-c", script}) || !reflect.DeepEqual(c.Env, wantEnv) ||
		len(c.EnvFrom) != 0 || !reflect.DeepEqual(c.SecurityContext, hardenedSecurityContext()) {
		t.Errorf("retention container = %+v", c)
	}
	for _, want := range []string{
		"ALTER TABLE default.oramacore_analytics_search_raw MODIFY TTL timestamp + INTERVAL 30 DAY",
		"ALTER TABLE default.oramacore_analytics_interaction_raw MODIFY TTL timestamp + INTERVAL 30 DAY",
		"ALTER TABLE default.oramacore_analytics_search_aggregated MODIFY TTL created_at + INTERVAL 30 DAY",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the retention script lacks %q", want)
		}
	}
	if got := retentionStatus(t, h); got != (oramav1alpha1.ComponentStatus{Name: "analytics-retention", Detail: "pending"}) {
		t.Errorf("retention status = %+v", got)
	}
}

// retentionStatus is the CR status's analytics-retention entry.
func retentionStatus(t *testing.T, h *engineHarness) oramav1alpha1.ComponentStatus {
	t.Helper()
	for _, c := range h.stored().Status.Components {
		if c.Name == "analytics-retention" {
			return c
		}
	}
	t.Fatal("no analytics-retention status entry")
	return oramav1alpha1.ComponentStatus{}
}

// A failed or outdated retention Job is replaced; a succeeded one is kept and reported applied.
func TestAnalyticsRetentionJobIsReplacedWhenFailedOrOutdated(t *testing.T) {
	h := newImportHarness(t, importOC(nil))
	// update applies mut to the Job's metadata, then to its status.
	update := func(mut func(*batchv1.Job)) {
		t.Helper()
		for _, write := range []func(context.Context, *batchv1.Job) error{
			func(ctx context.Context, j *batchv1.Job) error { return h.cl.Update(ctx, j) },
			func(ctx context.Context, j *batchv1.Job) error { return h.cl.Status().Update(ctx, j) },
		} {
			job := &batchv1.Job{}
			h.get("v1-analytics-retention", job)
			mut(job)
			if err := write(context.Background(), job); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, mut := range map[string]func(*batchv1.Job){
		"failed": func(j *batchv1.Job) {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		},
		"outdated": func(j *batchv1.Job) { j.Annotations[analyticsRetentionHashKey] = "an older script" },
	} {
		// The first reconcile deletes the Job, the second creates it again.
		update(mut)
		h.mustRun()
		if h.get("v1-analytics-retention", &batchv1.Job{}) {
			t.Errorf("a %s Job was kept", name)
		}
		h.mustRun()
		job := &batchv1.Job{}
		if !h.get("v1-analytics-retention", job) || jobFailed(job) || job.Annotations[analyticsRetentionHashKey] != templateSHA256(analyticsRetentionScript(h.stored())) {
			t.Errorf("%s Job was not replaced: %+v", name, job)
		}
	}
	update(func(j *batchv1.Job) { j.Status.Succeeded = 1 })
	before := &batchv1.Job{}
	h.get("v1-analytics-retention", before)
	h.mustRun()
	writes := h.statusWrites
	h.mustRun()
	after := &batchv1.Job{}
	if !h.get("v1-analytics-retention", after) || after.UID != before.UID {
		t.Error("a succeeded retention Job was replaced")
	}
	if got := retentionStatus(t, h); got != (oramav1alpha1.ComponentStatus{Name: "analytics-retention", Ready: true, Detail: "applied"}) {
		t.Errorf("retention status = %+v", got)
	}
	if h.statusWrites != writes {
		t.Error("a steady reconcile wrote the status")
	}
	h.updateCR(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Analytics.Enabled = false
		o.Spec.Components.AnalyticsProcessor.Import.Enabled = false
	})
	h.mustRun()
	if h.get("v1-analytics-retention", &batchv1.Job{}) {
		t.Error("the retention Job survived disabling the in-cluster ClickHouse")
	}
}

// fakeClickHouseClient answers the retention script's queries from files in $STATE: table.<name>
// holds a table's engine_full, and every query is appended to queries.
const fakeClickHouseClient = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --password) printf '%s' "$2" > "$STATE/password"; shift ;;
    --query) q=$2; shift ;;
  esac
  shift
done
printf '%s\n' "$q" >> "$STATE/queries"
case "$q" in
  "EXISTS TABLE default."*) if [ -f "$STATE/table.${q#EXISTS TABLE default.}" ]; then echo 1; else echo 0; fi ;;
  "SELECT engine_full"*) t=$(printf '%s' "$q" | sed "s/.*name = '\([^']*\)'.*/\1/"); cat "$STATE/table.$t" ;;
esac
`

// The retention script against a fake clickhouse-client: it fails while a table is missing, sets
// each TTL that differs, and leaves one already at 30 days alone.
func TestAnalyticsRetentionScriptSetsOnlyDifferentTTLs(t *testing.T) {
	const password = `p'a"ss$(touch pwned)`
	const plain = "MergeTree ORDER BY id SETTINGS index_granularity = 8192"
	for name, tc := range map[string]struct {
		tables  map[string]string
		fails   bool
		altered []string
	}{
		"tables missing": {tables: map[string]string{analyticsSearchTable: plain}, fails: true},
		"no TTL": {tables: map[string]string{analyticsSearchTable: plain, analyticsInteractionTable: plain, analyticsAggregatedTable: plain},
			altered: []string{analyticsSearchTable, analyticsInteractionTable, analyticsAggregatedTable}},
		"already 30 days, or 300": {tables: map[string]string{
			analyticsSearchTable:      "MergeTree ORDER BY id TTL timestamp + toIntervalDay(30) SETTINGS index_granularity = 8192",
			analyticsInteractionTable: "MergeTree ORDER BY id TTL timestamp + toIntervalDay(300) SETTINGS index_granularity = 8192",
			analyticsAggregatedTable:  "MergeTree ORDER BY created_at TTL created_at + toIntervalDay(30)"},
			altered: []string{analyticsInteractionTable}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state, bin := filepath.Join(dir, "state"), filepath.Join(dir, "bin")
			for _, d := range []string{state, bin} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bin, "clickhouse-client"), []byte(fakeClickHouseClient), 0o755); err != nil {
				t.Fatal(err)
			}
			for table, engine := range tc.tables {
				if err := os.WriteFile(filepath.Join(state, "table."+table), []byte(engine), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", analyticsRetentionScript(newOCV1(nil)))
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "STATE=" + state, "CLICKHOUSE_PASSWORD=" + password}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fails {
				t.Fatalf("script error = %v, want failure %v: %s", err, tc.fails, out)
			}
			queries, _ := os.ReadFile(filepath.Join(state, "queries"))
			var altered []string
			for _, q := range strings.Split(strings.TrimSpace(string(queries)), "\n") {
				if rest, ok := strings.CutPrefix(q, "ALTER TABLE default."); ok {
					altered = append(altered, strings.Fields(rest)[0])
				}
			}
			slices.Sort(altered)
			want := slices.Clone(tc.altered)
			slices.Sort(want)
			if !reflect.DeepEqual(altered, want) {
				t.Errorf("altered %v, want %v", altered, want)
			}
			if got, _ := os.ReadFile(filepath.Join(state, "password")); string(got) != password {
				t.Errorf("clickhouse-client got password %q", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
				t.Error("the script executed the password")
			}
		})
	}
}
