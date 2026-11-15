/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// The Altinity ClickHouse operator (CHOP) signs in to every ClickHouse server it runs as its own
// user, clickhouse_operator. Chart 0.27.4 ships that user's password as a fixed value, the same on
// every install, limited only by the CHOP pod's source address (operator#772). The Orama operator
// mints one generated credential per cluster instead, Secret clickhouse-operator-credentials in
// clickhouse-system (keys username and password), once, and never rotates it by itself.
//
// It runs on every cluster, the same everywhere: it has no setting. CHOP reads the Secret only
// once the chart values name it (secret.create false and the operator config's
// clickhouse.access.secret pointing here), which is a change to helmfile.yaml and every
// platform.yaml together (docs/lake/services.md, "Built-in users").
const (
	clickHouseOperatorNamespace        = "clickhouse-system"
	clickHouseOperatorCredentialSecret = "clickhouse-operator-credentials"
	clickHouseOperatorUser             = "clickhouse_operator"
	// clickHouseOperatorCredentialCheck is how often the Secret is looked for, so a namespace the
	// platform creates later, or a deleted Secret, is found.
	clickHouseOperatorCredentialCheck = 5 * time.Minute
)

// ClickHouseOperatorCredential is a manager Runnable that keeps the Secret above.
type ClickHouseOperatorCredential struct {
	Client client.Client
	// Reader reads without the cache (no cluster-wide Secret informer is needed for one Secret).
	Reader client.Reader
}

// NeedLeaderElection: only the leader writes.
func (c *ClickHouseOperatorCredential) NeedLeaderElection() bool { return true }

// Start keeps the Secret until the manager stops.
func (c *ClickHouseOperatorCredential) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("clickhouse-operator-credential")
	ticker := time.NewTicker(clickHouseOperatorCredentialCheck)
	defer ticker.Stop()
	for {
		if err := c.ensure(ctx); err != nil {
			log.Info("the ClickHouse operator's credential is not written yet; tried again later", "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ensure creates the Secret when it is missing. An existing one is never changed: changing the
// password would sign CHOP out of every ClickHouse server until each picks up the new hash. A
// missing namespace (the platform has not installed CHOP yet) is waited for, never created.
func (c *ClickHouseOperatorCredential) ensure(ctx context.Context) error {
	reader := c.Reader
	if reader == nil {
		reader = c.Client
	}
	key := client.ObjectKey{Namespace: clickHouseOperatorNamespace, Name: clickHouseOperatorCredentialSecret}
	var existing corev1.Secret
	if err := reader.Get(ctx, key, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: clickHouseOperatorNamespace}, &namespace); err != nil {
		return err
	}
	password, err := randomHex(32)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "orama-operator"},
			Annotations: map[string]string{
				"orama.cloud/purpose": "The Altinity ClickHouse operator's own clickhouse_operator credential, generated once per cluster (operator#772).",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"username": []byte(clickHouseOperatorUser), "password": []byte(password)},
	}
	if err := c.Client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
