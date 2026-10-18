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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func (h *engineHarness) service(name string) *corev1.Service {
	h.t.Helper()
	svc := &corev1.Service{}
	if !h.get(name, svc) {
		h.t.Fatalf("Service %s not found", name)
	}
	return svc
}

func TestReadersRunAsStatefulSetWithOneDataVolumeEach(t *testing.T) {
	h := newEngineHarness(t)
	limits := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")}}
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Images.PullSecretName = "regcred"
		o.Spec.Components.Engine.Reader.Replicas = 2
		o.Spec.Components.Engine.Reader.Resources = &limits
	})
	h.reconcile(oc)
	first := h.reader()
	h.reconcile(oc)
	sts := h.reader()
	if sts.ResourceVersion != first.ResourceVersion || !reflect.DeepEqual(sts.Spec, first.Spec) {
		t.Error("a second identical reconcile changed the reader StatefulSet")
	}

	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		t.Errorf("podManagementPolicy = %q, want Parallel", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.PersistentVolumeClaimRetentionPolicy != nil {
		t.Errorf("persistentVolumeClaimRetentionPolicy = %+v, want unset (Kubernetes' Retain)", sts.Spec.PersistentVolumeClaimRetentionPolicy)
	}
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("volumeClaimTemplates = %d, want 1", n)
	}
	pvc := sts.Spec.VolumeClaimTemplates[0]
	if pvc.Name != "data" || !reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) ||
		!pvc.Spec.Resources.Requests.Storage().Equal(resource.MustParse(readerDataSize)) {
		t.Errorf("claim template = %s %v %s, want data ReadWriteOnce %s",
			pvc.Name, pvc.Spec.AccessModes, pvc.Spec.Resources.Requests.Storage(), readerDataSize)
	}

	pod := sts.Spec.Template.Spec
	for _, v := range pod.Volumes {
		if v.Name == "data" {
			t.Errorf("the pod declares volume data (%+v); it must come from the claim template", v.VolumeSource)
		}
	}
	core := container(pod, "oramacore")
	mounted := false
	for _, m := range core.VolumeMounts {
		mounted = mounted || (m.Name == "data" && m.MountPath == "/app/.data")
	}
	if !mounted {
		t.Errorf("oramacore mounts %+v, want data at /app/.data", core.VolumeMounts)
	}

	if !core.Resources.Limits.Memory().Equal(resource.MustParse("3Gi")) {
		t.Errorf("reader memory limit = %s, want the 3Gi override", core.Resources.Limits.Memory())
	}
	var inits []string
	for _, c := range pod.InitContainers {
		inits = append(inits, c.Name)
	}
	want := []string{waitForBrokerInit(oc).Name, renderConfigInit(oc, engineSideFor(oc, "reader")).Name}
	if !reflect.DeepEqual(inits, want) {
		t.Errorf("init containers = %v, want %v", inits, want)
	}
	if !reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "regcred"}}) {
		t.Errorf("imagePullSecrets = %v, want regcred", pod.ImagePullSecrets)
	}
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != readerGracePeriodSeconds {
		t.Errorf("grace period = %v, want %d", pod.TerminationGracePeriodSeconds, readerGracePeriodSeconds)
	}
	if !reflect.DeepEqual(pod.TopologySpreadConstraints, spreadConstraints(labelsFor(oc, "reader"))) {
		t.Errorf("topology spread = %+v, want the reader spread for 2 replicas", pod.TopologySpreadConstraints)
	}
}

func TestReaderHeadlessServiceGovernsStatefulSet(t *testing.T) {
	for _, envoy := range []bool{false, true} {
		name := "without Envoy"
		target := int32(oramacorePort)
		if envoy {
			name, target = "with Envoy", envoyPort
		}
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t)
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.Envoy = envoy
				o.Spec.Components.Analytics.Enabled = false
			})
			h.reconcile(oc)
			if got := h.reader().Spec.ServiceName; got != "v1-reader-headless" {
				t.Fatalf("serviceName = %q, want v1-reader-headless", got)
			}

			headless := h.service("v1-reader-headless")
			lbls := labelsFor(oc, "reader")
			if headless.Spec.ClusterIP != corev1.ClusterIPNone || headless.Spec.PublishNotReadyAddresses ||
				!reflect.DeepEqual(headless.Spec.Selector, lbls) || !reflect.DeepEqual(headless.Labels, lbls) {
				t.Errorf("headless Service = clusterIP %q publishNotReady %v selector %v labels %v, want None false and the reader labels",
					headless.Spec.ClusterIP, headless.Spec.PublishNotReadyAddresses, headless.Spec.Selector, headless.Labels)
			}
			wantPorts := []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt32(8080)}}
			if !reflect.DeepEqual(headless.Spec.Ports, wantPorts) {
				t.Errorf("headless ports = %+v, want %+v", headless.Spec.Ports, wantPorts)
			}
			if !metav1.IsControlledBy(headless, oc) {
				t.Error("the headless Service is not controlled by the CR")
			}

			normal := h.service("v1-reader")
			if normal.Spec.ClusterIP == corev1.ClusterIPNone {
				t.Error("the reader Service became headless")
			}
			wantPorts = []corev1.ServicePort{{Name: "http", Port: envoyPort, TargetPort: intstr.FromInt32(target)}}
			if !reflect.DeepEqual(normal.Spec.Ports, wantPorts) {
				t.Errorf("reader Service ports = %+v, want %+v", normal.Spec.Ports, wantPorts)
			}

			oc.Spec.Components.Analytics.Enabled = true
			h.reconcile(oc)
			if got := h.reader().Spec.ServiceName; got != "v1-reader-headless" {
				t.Errorf("serviceName with analytics enabled = %q, want v1-reader-headless", got)
			}
		})
	}
}

func TestReaderReplicaCountIsFixedAndNoAutoscalerIsCreated(t *testing.T) {
	h := newEngineHarness(t)
	oc := engineOC(nil)
	h.reconcile(oc)
	if got := *h.reader().Spec.Replicas; got != 3 {
		t.Errorf("default replicas = %d, want 3", got)
	}

	oc.Spec.Components.Engine.Reader.Replicas = 2
	oc.Spec.Components.Engine.Reader.HPA = &oramav1alpha1.HPASpec{MinReplicas: 2, MaxReplicas: 6, TargetCPUUtilization: 70}
	h.reconcile(oc)
	if got := *h.reader().Spec.Replicas; got != 2 {
		t.Errorf("replicas = %d, want reader.replicas 2", got)
	}
	var hpas autoscalingv2.HorizontalPodAutoscalerList
	if err := h.cl.List(context.Background(), &hpas, client.InNamespace("orama-system")); err != nil {
		t.Fatal(err)
	}
	if len(hpas.Items) != 0 {
		t.Errorf("found %d HorizontalPodAutoscalers, want none even with reader.hpa set", len(hpas.Items))
	}
}

// readerDeployment is a Deployment named like the readers that earlier operator versions ran.
func readerDeployment(oc *oramav1alpha1.OramaCloudV1, owner *metav1.OwnerReference) *appsv1.Deployment {
	lbls := labelsFor(oc, "reader")
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: "orama-system", Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(3),
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbls}},
		},
	}
	if owner != nil {
		dep.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return dep
}

func TestOldReaderDeploymentIsDeleted(t *testing.T) {
	ctx := context.Background()
	oc := engineOC(nil)

	t.Run("controlled by the CR", func(t *testing.T) {
		dep := readerDeployment(oc, metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1")))
		// The garbage collector's finalizer keeps the Deployment while its pods go, as on a cluster.
		dep.Finalizers = []string{metav1.FinalizerDeleteDependents}
		s := ocv1TestScheme(t)
		var deletes []client.DeleteOptions
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey})).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					var o client.DeleteOptions
					o.ApplyOptions(opts)
					deletes = append(deletes, o)
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
		r := &OramaCloudV1Reconciler{Client: cl, Scheme: s}
		key := types.NamespacedName{Name: "v1-reader", Namespace: "orama-system"}

		for range 2 {
			if err := r.reconcileEngine(ctx, oc); err == nil {
				t.Fatal("reconcileEngine succeeded while the reader Deployment was being deleted")
			}
			if err := cl.Get(ctx, key, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatalf("reader StatefulSet exists while the Deployment is being deleted (get: %v)", err)
			}
		}
		if len(deletes) != 1 || deletes[0].PropagationPolicy == nil || *deletes[0].PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatalf("Deployment deletes = %+v, want one with foreground propagation", deletes)
		}

		// The garbage collector finishes: the Deployment is gone.
		pending := &appsv1.Deployment{}
		if err := cl.Get(ctx, key, pending); err != nil || pending.DeletionTimestamp.IsZero() {
			t.Fatalf("pending Deployment = %v (get: %v), want it marked for deletion", pending.DeletionTimestamp, err)
		}
		pending.Finalizers = nil
		if err := cl.Update(ctx, pending); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileEngine(ctx, oc); err != nil {
			t.Fatalf("reconcileEngine after the Deployment was removed: %v", err)
		}
		if err := cl.Get(ctx, key, &appsv1.StatefulSet{}); err != nil {
			t.Errorf("reader StatefulSet after the Deployment was removed: %v", err)
		}
	})

	for name, owner := range map[string]*metav1.OwnerReference{
		"owned by someone else": metav1.NewControllerRef(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "uid-other"}},
			appsv1.SchemeGroupVersion.WithKind("Deployment")),
		"with no owner": nil,
	} {
		t.Run(name, func(t *testing.T) {
			dep := readerDeployment(oc, owner)
			h := newEngineHarness(t, dep)
			err := h.r.reconcileEngine(ctx, oc)
			if !apierrors.IsConflict(err) {
				t.Fatalf("reconcileEngine = %v, want a conflict", err)
			}
			if h.deletes != 0 {
				t.Errorf("deletes = %d, want the foreign Deployment left alone", h.deletes)
			}
			got := &appsv1.Deployment{}
			if !h.get("v1-reader", got) || !reflect.DeepEqual(got.OwnerReferences, dep.OwnerReferences) || !got.DeletionTimestamp.IsZero() {
				t.Error("the foreign Deployment was changed")
			}
			if h.get("v1-reader", &appsv1.StatefulSet{}) {
				t.Error("the reader StatefulSet was created next to a foreign Deployment")
			}
		})
	}
}

// Without a usable master key the engine is held: a reader Deployment from an earlier operator
// version is scaled to zero like the StatefulSets, and no reader StatefulSet is created.
func TestEngineHoldScalesOldReaderDeployment(t *testing.T) {
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.MasterAPIKeySecretRef = nil })
	dep := readerDeployment(oc, metav1.NewControllerRef(oc, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1")))
	h := newEngineHarness(t, dep)
	if err := h.r.reconcileEngine(context.Background(), oc); err != nil {
		t.Fatalf("reconcileEngine: %v", err)
	}
	got := &appsv1.Deployment{}
	if !h.get("v1-reader", got) || replicasOf(got.Spec.Replicas) != 0 {
		t.Errorf("old reader Deployment replicas = %d, want it kept at 0", replicasOf(got.Spec.Replicas))
	}
	if h.get("v1-reader", &appsv1.StatefulSet{}) {
		t.Error("the reader StatefulSet was created without a usable master key")
	}
}

func TestReaderServiceAndDisruptionBudgetSelectStatefulSetReaders(t *testing.T) {
	h := newEngineHarness(t)
	h.reconcile(engineOC(nil))
	sts := h.reader()
	pods := labels.Set(sts.Spec.Template.Labels)
	sel, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
	if err != nil || !sel.Matches(pods) {
		t.Fatalf("the StatefulSet selector %v does not select its own pods %v (%v)", sts.Spec.Selector, pods, err)
	}

	svc := h.service("v1-reader")
	if !labels.SelectorFromSet(svc.Spec.Selector).Matches(pods) {
		t.Errorf("reader Service selector %v does not select reader pods %v", svc.Spec.Selector, pods)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if !h.get("v1-reader", pdb) {
		t.Fatal("reader PDB not found")
	}
	pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil || !pdbSel.Matches(pods) {
		t.Errorf("reader PDB selector %v does not select reader pods %v (%v)", pdb.Spec.Selector, pods, err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Errorf("reader PDB maxUnavailable = %v, want 1", pdb.Spec.MaxUnavailable)
	}
}

func TestEngineTeardownRemovesReaderStatefulSetAndKeepsClaims(t *testing.T) {
	oc := engineOC(nil)
	var claims []client.Object
	for _, n := range []string{"data-v1-reader-0", "data-v1-reader-1"} {
		claims = append(claims, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: n, Namespace: "orama-system", Labels: labelsFor(oc, "reader")}})
	}
	h := newEngineHarness(t, claims...)
	h.reconcile(oc)

	oc.Spec.Components.Engine.Enabled = false
	h.reconcile(oc)
	for _, o := range []struct {
		name string
		obj  client.Object
	}{
		{"v1-reader", &appsv1.StatefulSet{}},
		{"v1-reader", &corev1.Service{}},
		{"v1-reader-headless", &corev1.Service{}},
		{"v1-reader", &policyv1.PodDisruptionBudget{}},
	} {
		if h.get(o.name, o.obj) {
			t.Errorf("%T %s survived engine teardown", o.obj, o.name)
		}
	}
	for _, c := range claims {
		if !h.get(c.GetName(), &corev1.PersistentVolumeClaim{}) {
			t.Errorf("claim %s was deleted by engine teardown", c.GetName())
		}
	}
}
