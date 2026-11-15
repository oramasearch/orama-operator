/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The ClickHouse operator's credential is generated once, waits for its namespace, and is never
// changed afterwards (operator#772).
func TestClickHouseOperatorCredentialIsGeneratedOnceAndKept(t *testing.T) {
	ctx := context.Background()
	s := newLakeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	credential := &ClickHouseOperatorCredential{Client: c}
	key := client.ObjectKey{Namespace: clickHouseOperatorNamespace, Name: clickHouseOperatorCredentialSecret}

	if err := credential.ensure(ctx); err == nil {
		t.Fatal("the credential was written before clickhouse-system existed")
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: clickHouseOperatorNamespace}}); err != nil {
		t.Fatal(err)
	}
	if err := credential.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	var first corev1.Secret
	if err := c.Get(ctx, key, &first); err != nil {
		t.Fatal(err)
	}
	if string(first.Data["username"]) != clickHouseOperatorUser || len(first.Data["password"]) != 64 {
		t.Fatalf("credential = %q / %d bytes", first.Data["username"], len(first.Data["password"]))
	}
	if string(first.Data["password"]) == "clickhouse_operator_password" {
		t.Fatal("the chart's fixed password was used")
	}
	if err := credential.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	var second corev1.Secret
	if err := c.Get(ctx, key, &second); err != nil {
		t.Fatal(err)
	}
	if string(second.Data["password"]) != string(first.Data["password"]) {
		t.Fatal("the credential was rotated; it is minted once")
	}
}
