/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// deleteCloudV1Owned removes only this CR's resource; foreign objects are never adopted on disable.
func (r *OramaCloudV1Reconciler) deleteCloudV1Owned(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, oc) {
		return fmt.Errorf("refusing to delete unowned %T %s", obj, obj.GetName())
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}

// reconcileEngineIngresses publishes the writer (primary region only) and the readers on their hosts,
// each with a companion <name>-metrics Ingress that keeps /metrics private (upsertMetricsBlockIngress),
// and removes both Ingresses of an endpoint without a host.
func (r *OramaCloudV1Reconciler) reconcileEngineIngresses(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	engine := oc.Spec.Components.Engine
	for _, endpoint := range []struct {
		component, host string
		ingress         oramav1alpha1.IngressHostsSpec
		port            int32
	}{
		{"writer", engine.Writer.Host, engine.Writer.IngressHostsSpec, oramacorePort},
		{"reader", engine.Reader.Host, engine.Reader.IngressHostsSpec, envoyPort},
	} {
		host := endpoint.host
		if !oc.Spec.Components.Engine.Enabled || (endpoint.component == "writer" && oc.Spec.Region.Role != "primary") {
			host = ""
		}
		name := oc.Name + "-" + endpoint.component
		if host == "" {
			for _, n := range []string{name, name + "-metrics"} {
				if err := r.deleteCloudV1Owned(ctx, oc, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: oc.Namespace}}); err != nil {
					return err
				}
			}
			continue
		}
		if err := r.upsertIngress(ctx, oc, name, endpoint.component, host, endpoint.ingress, endpoint.port); err != nil {
			return err
		}
		if err := r.upsertMetricsBlockIngress(ctx, oc, name, endpoint.component, host, endpoint.ingress, endpoint.port); err != nil {
			return err
		}
	}
	return nil
}

// validateIngressHosts refuses a CR whose Ingresses would claim one name twice: every host and extra
// host of the dashboard, docs, writer and reader must be unique across the CR (enabled or not), and
// extra hosts need a host. ingress-nginx would otherwise merge or pick between the rules silently.
func validateIngressHosts(oc *oramav1alpha1.OramaCloudV1) error {
	c := oc.Spec.Components
	owners := map[string]string{}
	for _, component := range []struct {
		field, host string
		ingress     oramav1alpha1.IngressHostsSpec
	}{
		{"dashboard", c.Dashboard.Host, c.Dashboard.IngressHostsSpec},
		{"docs", c.Docs.Host, c.Docs.IngressHostsSpec},
		{"engine.writer", c.Engine.Writer.Host, c.Engine.Writer.IngressHostsSpec},
		{"engine.reader", c.Engine.Reader.Host, c.Engine.Reader.IngressHostsSpec},
	} {
		if component.host == "" && (len(component.ingress.ExtraHosts) > 0 || component.ingress.TLSSecretName != "") {
			return fmt.Errorf("%s.extraHosts and %s.tlsSecretName require %s.host", component.field, component.field, component.field)
		}
		claim := func(host, field string) error {
			key := strings.ToLower(host)
			if owner, taken := owners[key]; taken {
				return fmt.Errorf("host %q is set in both %s and %s; each host may route to one component only", host, owner, field)
			}
			owners[key] = field
			return nil
		}
		if component.host != "" {
			if err := claim(component.host, component.field+".host"); err != nil {
				return err
			}
		}
		for _, extra := range component.ingress.ExtraHosts {
			if extra == "" {
				return fmt.Errorf("%s.extraHosts has an empty entry", component.field)
			}
			if strings.EqualFold(extra, component.host) {
				return fmt.Errorf("%s.extraHosts repeats %s.host %q", component.field, component.field, extra)
			}
			if err := claim(extra, component.field+".extraHosts"); err != nil {
				return err
			}
		}
	}
	return nil
}
