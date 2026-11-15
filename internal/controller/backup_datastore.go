/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// What the backup system needs to know about a datastore's pods (operator#740): which pods they
// are, and whether the server itself writes to the bucket. The workspace and stack namespaces deny
// every connection by default (docs/infrastructure/network-sandbox.md), so a run opens exactly
// two things, for its own lifetime and owned by the run: the datastore's pods accept the run's
// pod, and the pods of a kind whose server writes to the bucket reach public HTTPS on 443. Both
// go when the run finishes.

// publicHTTPSExcept are the ranges a "public HTTPS" egress rule never reaches: private,
// link-local (cloud metadata) and carrier-grade NAT.
var publicHTTPSExcept = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

// serverWritesToBucket: the kinds whose own server (or its operator's agent beside it) writes the
// backup to the bucket, rather than the run's pod: ClickHouse's BACKUP ... TO S3, TiDB's BR on
// TiKV, Cassandra's Medusa beside each node.
func serverWritesToBucket(kind oramav1alpha1.BackupDatastoreKind) bool {
	switch kind {
	case oramav1alpha1.BackupDatastoreClickHouse, oramav1alpha1.BackupDatastoreTiDB, oramav1alpha1.BackupDatastoreCassandra:
		return true
	}
	return false
}

// datastorePodSelector is the datastore's pods: spec.datastore.podSelector, or the labels the
// kind's operator puts on the pods of a datastore of this name. Nil means unknown, and no policy is
// written for the datastore.
func datastorePodSelector(store oramav1alpha1.BackupDatastoreRef) map[string]string {
	if len(store.PodSelector) > 0 {
		return store.PodSelector
	}
	switch store.Kind {
	case oramav1alpha1.BackupDatastorePostgres:
		return map[string]string{"cnpg.io/cluster": store.Name}
	case oramav1alpha1.BackupDatastoreClickHouse:
		return map[string]string{"clickhouse.altinity.com/chi": store.Name}
	case oramav1alpha1.BackupDatastoreTiDB:
		return map[string]string{"app.kubernetes.io/instance": store.Name}
	case oramav1alpha1.BackupDatastoreValkey:
		return map[string]string{valkeyClusterLabel: store.Name}
	case oramav1alpha1.BackupDatastoreZetta:
		return map[string]string{"app.kubernetes.io/name": "orama-zetta", "app.kubernetes.io/instance": store.Name}
	}
	return nil
}

// connectionEnv is the datastore connection the run's container reads, from
// spec.datastore.connectionSecretRef; none when the kind needs none.
func connectionEnv(store oramav1alpha1.BackupDatastoreRef, fromSecret func(env, secret, key string, optional bool) corev1.EnvVar) []corev1.EnvVar {
	if store.ConnectionSecretRef == nil || store.ConnectionSecretRef.Name == "" {
		return nil
	}
	connection := store.ConnectionSecretRef.Name
	return []corev1.EnvVar{
		fromSecret(backup.EnvDatastoreHost, connection, "host", false),
		fromSecret(backup.EnvDatastorePort, connection, "port", true),
		fromSecret(backup.EnvDatastoreUsername, connection, "username", true),
		fromSecret(backup.EnvDatastorePassword, connection, "password", false),
		fromSecret(backup.EnvDatastoreDatabase, connection, "database", true),
	}
}

func datastoreRunPolicyName(jobName string) string { return jobName + "-ds" }

// ensureDatastoreRunPolicy writes the run's NetworkPolicy on the datastore's pods: ingress from the
// run's pod (selected by runPod), and for a kind whose server writes to the bucket, egress to
// public HTTPS. It is owned by the run (a Backup or a BackupRestore) and deleted when the run ends.
func ensureDatastoreRunPolicy(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, jobName string, store oramav1alpha1.BackupDatastoreRef, runPod map[string]string, labels map[string]string) error {
	selector := datastorePodSelector(store)
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: datastoreRunPolicyName(jobName), Namespace: owner.GetNamespace()}}
	if selector == nil {
		return deleteIfExists(ctx, c, policy)
	}
	tcp, https := corev1.ProtocolTCP, intstr.FromInt32(443)
	_, err := controllerutil.CreateOrUpdate(ctx, c, policy, func() error {
		if policy.ResourceVersion != "" && !metav1.IsControlledBy(policy, owner) {
			return fmt.Errorf("NetworkPolicy %s/%s exists and is not this run's", policy.Namespace, policy.Name)
		}
		policy.Labels = labels
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: runPod}}},
			}},
		}
		if serverWritesToBucket(store.Kind) {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
			policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: publicHTTPSExcept}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
			}}
		}
		return controllerutil.SetControllerReference(owner, policy, scheme)
	})
	return err
}

// backupConnectionData maps a Lake service's connection document (the JSON lake reads from the
// services Secret) to the keys a backup Job reads: host, port, username, password, database. The
// documents differ per kind: ClickHouse names its HTTP port http_port, Zetta and the wrappers give
// an endpoint and a token.
func backupConnectionData(credential map[string]interface{}) map[string][]byte {
	text := func(keys ...string) string {
		for _, key := range keys {
			switch value := credential[key].(type) {
			case string:
				if value != "" {
					return value
				}
			case float64:
				return fmt.Sprintf("%d", int64(value))
			}
		}
		return ""
	}
	data := map[string][]byte{}
	host, port := text("host"), text("http_port", "port")
	if host == "" {
		if endpoint := text("endpoint"); endpoint != "" {
			host, port = splitEndpoint(endpoint, port)
		}
	}
	for key, value := range map[string]string{
		"host": host, "port": port, "username": text("user", "username"),
		"password": text("password", "token"), "database": text("database"),
	} {
		if value != "" {
			data[key] = []byte(value)
		}
	}
	return data
}

// splitEndpoint reads host and port from "http://host:port/..." or "host:port".
func splitEndpoint(endpoint, fallbackPort string) (string, string) {
	rest := endpoint
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, ":"); i > 0 && !strings.Contains(rest[i:], "]") {
		return rest[:i], rest[i+1:]
	}
	return rest, fallbackPort
}
