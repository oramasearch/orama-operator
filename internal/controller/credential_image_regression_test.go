// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"regexp"
	"testing"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// This proof also runs unchanged against the pre-fix source: it uses only existing renderers.
func TestCredentialJobImagesCannotDrift(t *testing.T) {
	scheme := newLakeScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	z := &oramav1alpha1.OramaZettaCluster{ObjectMeta: metav1.ObjectMeta{Name: "digest-proof", Namespace: "test"}}
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	if err := r.reconcileZettaControlPlane(context.Background(), z, "zetta"); err != nil {
		t.Fatal(err)
	}
	zetta := &batchv1.Job{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: z.Namespace, Name: z.Name + "-zetta-schema"}, zetta); err != nil {
		t.Fatal(err)
	}
	jobs := []struct {
		name string
		spec batchv1.JobSpec
	}{
		{"restore", verificationJobSpec(&oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, "source", "restored", "source-db", true)},
		{"failover", verificationJobSpec(&oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, "source", "", "read-only", false)},
		{"postgres-schema", pgMigrationJobSpec(&oramav1alpha1.PostgresSchema{})},
		{"zetta-schema", zetta.Spec},
	}
	pin := regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	for _, job := range jobs {
		for _, container := range append(job.spec.Template.Spec.InitContainers, job.spec.Template.Spec.Containers...) {
			if !pin.MatchString(container.Image) {
				t.Errorf("%s/%s uses mutable image %q", job.name, container.Name, container.Image)
			}
		}
	}
}
