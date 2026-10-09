package controller

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestControlDBOwnershipRefusesForeignResources(t *testing.T) {
	for _, kind := range []string{"Cluster", "ScheduledBackup"} {
		for _, identity := range []string{"unowned", "foreign-controller", "retained-other-uid"} {
			for _, action := range []string{"reconcile", "delete", "release"} {
				t.Run(kind+"/"+identity+"/"+action, func(t *testing.T) {
					oc := fullStackOCV1(withBackup)
					oc.Spec.DeletionProtection = action == "release" || identity == "retained-other-uid"
					obj := newCNPGScheduledBackupFor(controlDBScheduledBackupName(oc), oc.Namespace)
					if kind == "Cluster" {
						obj.SetGroupVersionKind(cnpgClusterGVK)
						obj.SetName(controlDBClusterName(oc))
					}
					obj.SetLabels(labelsFor(oc, "controldb")) // same labels do not prove ownership
					obj.Object["spec"] = map[string]interface{}{"sentinel": "must-stay"}
					switch identity {
					case "foreign-controller":
						other := oc.DeepCopy()
						other.UID = types.UID("other-cr")
						obj.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(other, oramav1alpha1.GroupVersion.WithKind("OramaCloudV1"))})
					case "retained-other-uid":
						obj.SetAnnotations(map[string]string{cloudV1OwnerUIDAnnotation: "other-cr"})
					}
					scheme := ocv1TestScheme(t)
					cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj, backupSecret()).Build()
					r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
					before := &unstructured.Unstructured{}
					before.SetGroupVersionKind(obj.GroupVersionKind())
					if err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), before); err != nil {
						t.Fatal(err)
					}
					var err error
					switch action {
					case "reconcile":
						if kind == "Cluster" {
							err = r.reconcileControlDB(context.Background(), oc)
						} else {
							cfg, resolveErr := r.resolveControlDBBackup(context.Background(), oc)
							if resolveErr != nil {
								t.Fatal(resolveErr)
							}
							err = r.reconcileControlDBScheduledBackup(context.Background(), oc, cfg)
						}
					case "delete":
						if kind == "ScheduledBackup" {
							err = r.reconcileControlDBScheduledBackup(context.Background(), oc, cnpgBackupConfig{})
						} else {
							err = r.teardownControlDB(context.Background(), oc)
						}
					case "release":
						err = r.releaseFromCR(context.Background(), oc, obj)
					}
					if err == nil {
						t.Fatal("foreign resource was accepted")
					}
					after := before.DeepCopy()
					if err := cl.Get(context.Background(), client.ObjectKeyFromObject(before), after); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before.Object, after.Object) {
						t.Fatal("foreign resource changed")
					}
				})
			}
		}
	}
}

func TestControlDBRetainedOwnershipSurvivesDisableAndReenable(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		withBackup(o)
		o.Spec.DeletionProtection = true
	}), backupSecret())
	for _, mutate := range []func(*oramav1alpha1.OramaCloudV1){
		func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = false },
		func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ControlDB.Enabled = true },
		func(o *oramav1alpha1.OramaCloudV1) { o.Spec.DeletionProtection = false },
	} {
		reconcileAgain(t, cl, r, mutate)
		cluster := controlDBCluster(t, cl)
		sb, err := scheduledBackup(cl)
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range []client.Object{cluster, sb} {
			if obj.GetAnnotations()[cloudV1OwnerUIDAnnotation] != "uid-v1" {
				t.Fatalf("%s lost retained ownership", obj.GetName())
			}
		}
	}
	if !metav1.IsControlledBy(controlDBCluster(t, cl), getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})) {
		t.Fatal("removing protection did not restore controller ownership")
	}
}

func TestCloudV1TeardownAndReaderRetirementRefuseForeignDeployment(t *testing.T) {
	for _, action := range []string{"disable", "retire-reader"} {
		t.Run(action, func(t *testing.T) {
			oc := fullStackOCV1(nil)
			old := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: oc.Namespace, Labels: labelsFor(oc, "reader")}}
			scheme := ocv1TestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
			r := &OramaCloudV1Reconciler{Client: cl, Scheme: scheme}
			var err error
			if action == "disable" {
				err = r.teardownByComponent(context.Background(), oc, "reader")
			} else {
				sts := &appsv1.StatefulSet{ObjectMeta: old.ObjectMeta, Spec: appsv1.StatefulSetSpec{Replicas: int32Ptr(2)}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 2}}
				err = r.retireReaderDeployment(context.Background(), oc, sts)
			}
			if err == nil {
				t.Fatal("foreign Deployment deletion was accepted")
			}
			getObj(t, cl, "v1-reader", &appsv1.Deployment{})
		})
	}
}
