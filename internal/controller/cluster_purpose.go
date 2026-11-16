// SPDX-License-Identifier: MIT

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Cluster purposes. A KubernetesCluster's `purpose` tag is nonprod or production. The managing
// cluster has no KubernetesCluster of its own: an empty clusterRef names it, and it counts as
// production, because it already holds every production credential and manages the production
// clusters.

// managingClusterName is how a message names the cluster an empty clusterRef points at.
const managingClusterName = "the managing cluster"

// clusterPurpose is the purpose of the cluster clusterRef names, and whether that cluster exists.
// An empty clusterRef is the managing cluster itself, which is production.
func clusterPurpose(ctx context.Context, r client.Reader, clusterRef string) (string, bool, error) {
	if clusterRef == "" {
		return productionPurpose, true, nil
	}
	var cluster oramav1alpha1.KubernetesCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterRef}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return cluster.Spec.Tags[clusterPurposeTag], true, nil
}
