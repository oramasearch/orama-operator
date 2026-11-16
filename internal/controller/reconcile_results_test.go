// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// settle never returns a result with an error, and never hides an error: a conflict included.
func TestSettleReturnsAnErrorAloneAndHidesNothing(t *testing.T) {
	wanted := ctrl.Result{RequeueAfter: 30 * time.Second}
	if got, err := settle(wanted, nil); err != nil || got != wanted {
		t.Fatalf("no error: want the result unchanged, got %+v, %v", got, err)
	}
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "orama.cloud", Resource: "kubernetesclusterproviders"},
		"azure-orama", errors.New("the object has been modified"))
	for _, failure := range []error{errors.New("connection refused"), conflict} {
		got, err := settle(wanted, failure)
		if !errors.Is(err, failure) || got != (ctrl.Result{}) {
			t.Fatalf("%v: want the error returned with an empty result, got %+v, %v", failure, got, err)
		}
	}
}

// A controller owns its resource's status, so it writes status as a patch of what changed, with
// no version precondition. On 2026-10-12 the provider controller on the management cluster read a
// copy from its cache that was one write behind, wrote status with a full update, and the API
// refused it ("the object has been modified"). The cause was the update's version check, and the
// fix is to not make one: a status write from a stale read must succeed.
func TestStatusIsPatchedSoAStaleReadCannotConflict(t *testing.T) {
	ctx := context.Background()

	t.Run("KubernetesClusterProvider", func(t *testing.T) {
		c := clusterTestClient(t, testProvider())
		r := &KubernetesClusterProviderReconciler{Client: c, Scheme: clusterTestScheme(t)}
		var stale oramav1alpha1.KubernetesClusterProvider
		if err := c.Get(ctx, types.NamespacedName{Name: "azure-orama"}, &stale); err != nil {
			t.Fatal(err)
		}
		// Something writes the object after the read, as the controller's own finalizer does.
		fresh := stale.DeepCopy()
		fresh.Finalizers = []string{kubernetesClusterProviderFinalizer}
		if err := c.Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		observed := stale.Status.DeepCopy()
		stale.Status.Ready, stale.Status.AuthMethod = true, azureAuthWorkloadIdentity
		if err := r.writeStatus(ctx, &stale, observed); err != nil {
			t.Fatalf("a status write from a stale read was refused: %v", err)
		}
		var got oramav1alpha1.KubernetesClusterProvider
		if err := c.Get(ctx, types.NamespacedName{Name: "azure-orama"}, &got); err != nil {
			t.Fatal(err)
		}
		if !got.Status.Ready || got.Status.AuthMethod != azureAuthWorkloadIdentity {
			t.Fatalf("status not written: %+v", got.Status)
		}
		if len(got.Finalizers) != 1 {
			t.Fatalf("the status patch disturbed the finalizer written in between: %v", got.Finalizers)
		}
	})

	t.Run("KubernetesCluster", func(t *testing.T) {
		c := clusterTestClient(t, testKubernetesCluster())
		r := &KubernetesClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
		var stale oramav1alpha1.KubernetesCluster
		if err := c.Get(ctx, types.NamespacedName{Name: "example-nonprod"}, &stale); err != nil {
			t.Fatal(err)
		}
		fresh := stale.DeepCopy()
		fresh.Finalizers = []string{kubernetesClusterFinalizer}
		if err := c.Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		observed := stale.Status.DeepCopy()
		stale.Status.Phase = clusterPhasePending
		if err := r.writeStatus(ctx, &stale, observed); err != nil {
			t.Fatalf("a status write from a stale read was refused: %v", err)
		}
	})

	t.Run("AksCluster", func(t *testing.T) {
		c := clusterTestClient(t, testAksCluster(oramav1alpha1.ClusterDeletionRetain))
		r := &AksClusterReconciler{Client: c, Scheme: clusterTestScheme(t)}
		var stale oramav1alpha1.AksCluster
		if err := c.Get(ctx, types.NamespacedName{Name: "example-nonprod"}, &stale); err != nil {
			t.Fatal(err)
		}
		fresh := stale.DeepCopy()
		fresh.Finalizers = []string{aksClusterFinalizer}
		if err := c.Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		observed := stale.Status.DeepCopy()
		stale.Status.Phase = clusterPhaseProvisioning
		if err := r.writeStatus(ctx, &stale, observed); err != nil {
			t.Fatalf("a status write from a stale read was refused: %v", err)
		}
	})
}
