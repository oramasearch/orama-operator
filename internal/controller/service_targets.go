/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// serviceTarget is one Lake service as the cluster carries it: its kind, the name of its primary
// resource, and the labels that select the pods a connection or a tunnel to it reaches.
//
// A service's pods do not carry orama.cloud/service-id: upstream operators (CloudNativePG, TiDB
// Operator, Strimzi, ...) label their pods with their own keys, and copying ours onto them would
// roll every database pod. Its Kubernetes Services do not carry it either. Only the primary
// resource the Orama operator writes does, so a target is resolved from there.
type serviceTarget struct {
	Kind string
	Name string
	Pods map[string]string
}

// serviceTargetPrimaryGVKs are the resources that carry a service's id label, one per way a
// service is run. A wrapper kind's primary resource is its StatefulSet.
func serviceTargetPrimaryGVKs() []schema.GroupVersionKind {
	gvks := []schema.GroupVersionKind{cnpgClusterGVK, oramav1alpha1.GroupVersion.WithKind("OramaZettaCluster")}
	seen := map[schema.GroupVersionKind]bool{}
	for _, gvk := range gvks {
		seen[gvk] = true
	}
	names := make([]string, 0, len(catalogKinds))
	for name := range catalogKinds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		objects := catalogKinds[name].objects(catalogService{})
		if len(objects) == 0 || seen[objects[0].gvk] {
			continue
		}
		seen[objects[0].gvk] = true
		gvks = append(gvks, objects[0].gvk)
	}
	return gvks
}

// resolveServiceTarget finds the primary resource of Lake service id in namespace and derives the
// selector of its pods. A kind whose CRD is not installed is skipped.
func resolveServiceTarget(ctx context.Context, c client.Reader, lakeRef, namespace, id string) (serviceTarget, error) {
	for _, gvk := range serviceTargetPrimaryGVKs() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{serviceIDLabel: id}); err != nil {
			if isKindNotInstalled(err) {
				continue
			}
			return serviceTarget{}, err
		}
		for i := range list.Items {
			object := &list.Items[i]
			if !object.GetDeletionTimestamp().IsZero() {
				continue
			}
			kind := object.GetLabels()[serviceKindLabel]
			if gvk.Kind == "OramaZettaCluster" && kind == "" {
				kind = serviceKindZetta
			}
			pods, err := serviceTargetPods(kind, lakeRef, object.GetName(), id)
			if err != nil {
				return serviceTarget{}, err
			}
			return serviceTarget{Kind: kind, Name: object.GetName(), Pods: pods}, nil
		}
	}
	// No primary resource carries the id: fall back to pods that carry it themselves (an
	// OramaZettaCluster's and the wrapper kinds' pods do). The kind is then read from a Service
	// that carries the kind label, as before.
	kind, err := serviceKindFromServices(ctx, c, namespace, id)
	if err != nil {
		return serviceTarget{}, err
	}
	return serviceTarget{Kind: kind, Pods: map[string]string{serviceIDLabel: id}}, nil
}

// serviceKindFromServices reads a service's kind from a Kubernetes Service labelled with its id, or
// "" when there is none.
func serviceKindFromServices(ctx context.Context, c client.Reader, namespace, id string) (string, error) {
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(namespace), client.MatchingLabels{serviceIDLabel: id}); err != nil {
		return "", err
	}
	for i := range services.Items {
		if kind := services.Items[i].Labels[serviceKindLabel]; kind != "" {
			return kind, nil
		}
	}
	return "", nil
}

// serviceTargetPods is the selector of the pods that serve a service of kind, whose primary
// resource is name: the labels its upstream operator puts on them.
func serviceTargetPods(kind, lakeRef, name, id string) (map[string]string, error) {
	switch kind {
	case serviceKindZetta:
		return map[string]string{
			"app.kubernetes.io/component": "zetta-node",
			"app.kubernetes.io/instance":  name,
		}, nil
	case serviceKindPostgres:
		return map[string]string{cnpgClusterLabel: name}, nil
	}
	catalog, found := catalogKinds[kind]
	if !found {
		return nil, fmt.Errorf("service %s is of kind %q, which this operator does not connect to", id, kind)
	}
	in := catalogService{
		name: name,
		svc:  &LakeService{ID: id, Kind: kind, Slug: strings.TrimPrefix(name, lakeRef+"-")},
	}
	return catalog.podSelector(in), nil
}

// servicesSelecting returns the Services in namespace that select the pods pods selects: those
// whose spec.selector holds every one of its labels. A Service with no selector selects nothing.
func servicesSelecting(ctx context.Context, c client.Reader, namespace string, pods map[string]string) ([]corev1.Service, error) {
	var services corev1.ServiceList
	if err := c.List(ctx, &services, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var selecting []corev1.Service
	for _, service := range services.Items {
		if !service.DeletionTimestamp.IsZero() || len(service.Spec.Selector) == 0 || len(pods) == 0 {
			continue
		}
		matches := true
		for key, value := range pods {
			if service.Spec.Selector[key] != value {
				matches = false
				break
			}
		}
		if matches {
			selecting = append(selecting, service)
		}
	}
	return selecting, nil
}

// targetServices returns the Services of a target: those labelled with its pod selector (an
// app's own Service is) and those whose spec.selector holds all of it (an upstream operator's
// Services, which carry none of our labels). Each Service appears once.
func targetServices(ctx context.Context, c client.Reader, namespace string, target metav1.LabelSelector, selector labels.Selector) ([]corev1.Service, error) {
	var labelled corev1.ServiceList
	if err := c.List(ctx, &labelled, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	selecting, err := servicesSelecting(ctx, c, namespace, target.MatchLabels)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var services []corev1.Service
	for _, service := range append(labelled.Items, selecting...) {
		if seen[service.Name] || !service.DeletionTimestamp.IsZero() {
			continue
		}
		seen[service.Name] = true
		services = append(services, service)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services, nil
}
