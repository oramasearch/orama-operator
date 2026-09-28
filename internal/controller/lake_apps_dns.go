/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const lakeAppsDNSPurpose = "apps-wildcard"

func (r *LakeAppsReconciler) reconcileLakeAppsDNS(ctx context.Context, lake *oramav1alpha1.Lake) error {
	wildcard := strings.TrimPrefix(lake.Spec.Apps.Wildcard, "*.")
	name := ""
	if lake.Spec.Apps.Enabled && wildcard != "" && lake.Spec.DNSSecretRef != nil && lake.Spec.DNSSecretRef.Name != "" {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(lake.Name+"\n"+wildcard)))[:24]
		name = "lake-apps-" + hash
	}
	if err := r.deleteOldLakeAppsDNS(ctx, lake, name); err != nil || name == "" {
		return err
	}
	target, err := r.lakeAppsIngressTarget(ctx, lake)
	if err != nil || target == "" {
		return err
	}
	record := &oramav1alpha1.CloudflareDNSRecord{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lakeNamespace(lake)}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, record, func() error {
		if record.ResourceVersion != "" && !metav1.IsControlledBy(record, lake) {
			return fmt.Errorf("apps wildcard DNS record is not owned by Lake %s", lake.Name)
		}
		record.Labels = map[string]string{serviceLakeLabel: lake.Name, "orama.cloud/dns-purpose": lakeAppsDNSPurpose}
		record.Spec.SecretRef = lake.Spec.DNSSecretRef.Name
		record.Spec.Name = "*." + wildcard
		record.Spec.Content = target
		record.Spec.Proxied = true
		return controllerutil.SetControllerReference(lake, record, r.Scheme)
	})
	return err
}

func (r *LakeAppsReconciler) deleteOldLakeAppsDNS(ctx context.Context, lake *oramav1alpha1.Lake, keep string) error {
	var records oramav1alpha1.CloudflareDNSRecordList
	selector := client.MatchingLabels{serviceLakeLabel: lake.Name, "orama.cloud/dns-purpose": lakeAppsDNSPurpose}
	if err := r.List(ctx, &records, client.InNamespace(lakeNamespace(lake)), selector); err != nil {
		return err
	}
	for i := range records.Items {
		record := &records.Items[i]
		if record.Name == keep || !metav1.IsControlledBy(record, lake) || !record.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, record, client.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// Prefer the Lake ingress when it uses the app ingress class. Otherwise use an owned app's
// ingress, sorted by identity; this also supports apps on a Lake without public Lake ingress.
func (r *LakeAppsReconciler) lakeAppsIngressTarget(ctx context.Context, lake *oramav1alpha1.Lake) (string, error) {
	class := lake.Spec.Apps.IngressClass
	if class == "" {
		class = "nginx"
	}
	var ingress networkingv1.Ingress
	key := client.ObjectKey{Namespace: lakeNamespace(lake), Name: "lake-" + lake.Name}
	err := r.Get(ctx, key, &ingress)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	if err == nil && metav1.IsControlledBy(&ingress, lake) && ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName == class {
		if target := ingressLoadBalancerTarget(&ingress); target != "" {
			return target, nil
		}
	}
	var apps oramav1alpha1.LakeAppList
	if err := r.List(ctx, &apps, client.InNamespace(lakeNamespace(lake))); err != nil {
		return "", err
	}
	sort.Slice(apps.Items, func(i, j int) bool {
		return apps.Items[i].Name < apps.Items[j].Name
	})
	for i := range apps.Items {
		app := &apps.Items[i]
		if !metav1.IsControlledBy(app, lake) || !app.DeletionTimestamp.IsZero() {
			continue
		}
		key = client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppWorkloadName}
		if err := r.Get(ctx, key, &ingress); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return "", err
		}
		if ingress.Annotations[lakeAppOwnerAnnotation] != lakeAppOwner(app) || ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != class {
			continue
		}
		if target := ingressLoadBalancerTarget(&ingress); target != "" {
			return target, nil
		}
	}
	return "", nil
}
