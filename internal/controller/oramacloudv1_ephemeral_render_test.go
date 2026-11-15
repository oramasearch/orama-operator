/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func assertV1StorageBounds(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	for _, cs := range [][]corev1.Container{pod.Containers, pod.InitContainers} {
		for _, c := range cs {
			request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
			if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
				t.Errorf("container %s has invalid storage request=%s limit=%s", c.Name, request.String(), limit.String())
			}
		}
	}
	for _, v := range pod.Volumes {
		if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Sign() <= 0) {
			t.Errorf("volume %s has unbounded emptyDir", v.Name)
		}
	}
	for _, c := range pod.Containers {
		for _, mount := range c.VolumeMounts {
			if mount.Name == "rendered-config" && !mount.ReadOnly {
				t.Errorf("container %s can overwrite rendered config", c.Name)
			}
		}
	}
	for _, c := range pod.InitContainers {
		if c.Name == "render-config" || c.Name == "wait-for-broker" {
			if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
				t.Errorf("helper %s has writable image filesystem", c.Name)
			}
		}
	}
}

// Exercise the actual workload builders, including the overrides that formerly replaced defaults.
func TestV1RenderedStorageBounds(t *testing.T) {
	ctx := context.Background()
	for _, separate := range []bool{false, true} {
		for _, envoy := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				name := "combined"
				if separate {
					name = "separated"
				}
				if envoy {
					name += "-envoy"
				}
				if override {
					name += "-override"
				}
				t.Run(name, func(t *testing.T) {
					oc := newOCV1(nil)
					oc.Spec.Components.Engine.Envoy = envoy
					if separate {
						oc.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}
					}
					if override {
						oc.Spec.Components.Engine.Writer.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
						oc.Spec.Components.Engine.Reader.Resources = oc.Spec.Components.Engine.Writer.Resources.DeepCopy()
					}
					before := oc.DeepCopy()
					scheme := ocv1TestScheme(t)
					kube := fake.NewClientBuilder().WithScheme(scheme).Build()
					r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
					if err := r.reconcileWriter(ctx, oc, "", ""); err != nil {
						t.Fatal(err)
					}
					if err := r.reconcileReaders(ctx, oc, "", "", envoyConfig); err != nil {
						t.Fatal(err)
					}
					if err := r.reconcileEmbeddings(ctx, oc); err != nil {
						t.Fatal(err)
					}
					var sets appsv1.StatefulSetList
					if err := kube.List(ctx, &sets); err != nil {
						t.Fatal(err)
					}
					if len(sets.Items) != 2 {
						t.Fatalf("sets=%d", len(sets.Items))
					}
					for _, sts := range sets.Items {
						assertV1StorageBounds(t, sts.Spec.Template.Spec)
					}
					var deployments appsv1.DeploymentList
					if err := kube.List(ctx, &deployments); err != nil {
						t.Fatal(err)
					}
					for _, dep := range deployments.Items {
						assertV1StorageBounds(t, dep.Spec.Template.Spec)
					}
					if !reflect.DeepEqual(before, oc) {
						t.Fatal("rendering mutated the input CR")
					}
				})
			}
		}
	}
	t.Run("apps-stores-and-jobs", func(t *testing.T) {
		oc := newOCV1(nil)
		oc.Spec.Components.PgAdmin.Enabled = true
		oc.Spec.Components.Ingestion.Enabled = true
		oc.Spec.Components.AnalyticsProcessor.Enabled = true
		oc.Spec.Components.ControlDB.Enabled = true
		oc.Spec.DemoSeed = true
		scheme := ocv1TestScheme(t)
		kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(appliedGrantsSchema(t, oc, analyticsProcessorGrantsSchemaSuffix)).Build()
		r := &OramaCloudV1Reconciler{Client: kube, Scheme: scheme}
		for _, fn := range []func() error{
			func() error {
				return r.reconcileApp(ctx, oc, appDef{component: "app", image: "app:test", port: 8080, resources: &corev1.ResourceRequirements{},
					initContainers: []corev1.Container{{Name: "wait", Image: "busybox:1.37"}}})
			},
			func() error {
				return r.provisionStatefulSet(ctx, oc, dataStoreDef{component: "store", image: "store:test", dataPath: "/data", resources: &corev1.ResourceRequirements{},
					configTemplates: map[string]string{"config": "__TOKEN__"}, templateEnv: []corev1.EnvVar{{Name: "TOKEN", Value: "fixture"}}})
			},
			func() error { return r.reconcilePgAdmin(ctx, oc) },
			func() error { return r.reconcileHPES(ctx, oc) },
			func() error { return r.reconcileAnalyticsProcessor(ctx, oc) },
			func() error { return r.reconcileControlDB(ctx, oc) },
			func() error { return r.reconcileDemoSeed(ctx, oc) },
			func() error {
				return r.upsertCronJob(ctx, oc, "cron", "cron", "* * * * *", false, 300, func(p *corev1.PodSpec) {
					p.Containers = []corev1.Container{{Name: "cron", Image: "cron:test"}}
				})
			},
		} {
			if err := fn(); err != nil {
				t.Fatal(err)
			}
		}
		var deployments appsv1.DeploymentList
		if err := kube.List(ctx, &deployments); err != nil {
			t.Fatal(err)
		}
		if len(deployments.Items) != 4 {
			t.Fatalf("deployments=%d", len(deployments.Items))
		}
		for _, dep := range deployments.Items {
			assertV1StorageBounds(t, dep.Spec.Template.Spec)
		}
		var sets appsv1.StatefulSetList
		if err := kube.List(ctx, &sets); err != nil {
			t.Fatal(err)
		}
		for _, sts := range sets.Items {
			assertV1StorageBounds(t, sts.Spec.Template.Spec)
		}
		var jobs batchv1.JobList
		if err := kube.List(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs.Items {
			assertV1StorageBounds(t, job.Spec.Template.Spec)
		}
		var crons batchv1.CronJobList
		if err := kube.List(ctx, &crons); err != nil {
			t.Fatal(err)
		}
		for _, cron := range crons.Items {
			assertV1StorageBounds(t, cron.Spec.JobTemplate.Spec.Template.Spec)
		}
		cluster := newCNPGCluster(oc.Namespace, oc.Name+"-controldb")
		if err := kube.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"requests", "limits"} {
			value, found, err := unstructured.NestedString(cluster.Object, "spec", "resources", field, "ephemeral-storage")
			if err != nil || !found {
				t.Fatalf("CNPG storage %s missing: %v", field, err)
			}
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 {
				t.Fatalf("CNPG storage %s=%q", field, value)
			}
		}
		assertV1StorageBounds(t, analyticsRetentionJob(oc, "true", "fixture").Spec.Template.Spec)
		assertV1StorageBounds(t, controlDBClusterRowJob(oc).Spec.Template.Spec)
		m := migrationTestResource(nil)
		assertV1StorageBounds(t, migrationJob(m, &migrationTarget{oc: oc}, migrationRun{command: "inventory", jobName: "migration"}).Spec.Template.Spec)
	})
	t.Run("e2e-runner", func(t *testing.T) {
		kube, _ := reconcileE2E(t, e2eTestTarget(nil), e2eTestResource(""))
		var jobs batchv1.JobList
		if err := kube.List(ctx, &jobs, client.InNamespace("orama-system")); err != nil {
			t.Fatal(err)
		}
		if len(jobs.Items) != 1 {
			t.Fatalf("jobs=%d", len(jobs.Items))
		}
		assertV1StorageBounds(t, jobs.Items[0].Spec.Template.Spec)
	})
}
