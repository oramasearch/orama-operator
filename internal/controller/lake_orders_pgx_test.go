// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestCloudOrdersPgxStoreAgainstPostgres runs the pgx store's SQL on a real Postgres, in a
// database the test creates for itself and drops at the end (it never touches another):
//
//	LAKE_ORDERS_TEST_POSTGRES=1 [LAKE_ORDERS_TEST_POSTGRES_URI=postgres://127.0.0.1:5432/postgres] \
//	  go test ./internal/controller/ -run CloudOrdersPgx
//
// It proves the claim is exactly once with many connections at once and across a reconnect,
// that only the claimant moves an order on, and that no column but the operator's is written.
func TestCloudOrdersPgxStoreAgainstPostgres(t *testing.T) {
	if os.Getenv("LAKE_ORDERS_TEST_POSTGRES") != "1" {
		t.Skip("set LAKE_ORDERS_TEST_POSTGRES=1 to run the cloud_orders store against a local Postgres")
	}
	admin := os.Getenv("LAKE_ORDERS_TEST_POSTGRES_URI")
	if admin == "" {
		admin = "postgres://127.0.0.1:5432/postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = adminConn.Close(context.Background()) }()
	database := fmt.Sprintf("orama_operator_orders_test_%d", time.Now().UnixNano())
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatalf("create the test's own database: %v", err)
	}
	defer func() {
		// Only the database this test created.
		if _, err := adminConn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
	}()
	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	uri := parsed.String()

	open := func() CloudOrderStore {
		t.Helper()
		store, err := openPgxCloudOrderStore(ctx, uri, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close(context.Background()) })
		return store
	}
	const claimant = "orama-operator/LakeOrderSource/lake-live"

	// Before lake's migration: nothing is read, and the store says why.
	if _, err := open().ListOrders(ctx, claimant); !errors.Is(err, errCloudOrdersTableMissing) {
		t.Fatalf("without the table: %v, want errCloudOrdersTableMissing", err)
	}

	db, err := pgx.Connect(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	fixture, err := os.ReadFile("testdata/lake-cloud-orders.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(fixture)); err != nil {
		t.Fatalf("apply the cloud_orders contract: %v", err)
	}
	const (
		paid      = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		second    = "1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e"
		submitted = "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f"
	)
	// Lake's NOT NULL columns without a default are contact_name, contact_email and amount_cents;
	// a provider or region not given is '' (lake's default), never NULL.
	if _, err := db.Exec(ctx, `INSERT INTO cloud_orders (id, offer, path, cloud_provider, region, contact_name, contact_email, company, amount_cents, stripe_subscription_id, state) VALUES
		($1, 'private_warehouse', 'checkout', '', 'swedencentral', 'Ada', 'a@example.com', 'A', 100000, 'sub_a', 'paid'),
		($2, 'dedicated_cluster', 'checkout', 'azure', 'canadacentral', 'Ben', 'b@example.com', 'B', 500000, 'sub_b', 'paid'),
		($3, 'on_prem', 'order_form', '', '', 'Cy', 'c@example.com', 'C', 1000000, NULL, 'submitted')`, paid, second, submitted); err != nil {
		t.Fatal(err)
	}

	orders, err := open().ListOrders(ctx, claimant)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || orders[0].ID != paid || orders[0].Region != "swedencentral" || orders[1].CloudProvider != "azure" {
		t.Fatalf("listed %+v, want the two paid orders and not the submitted form", orders)
	}

	// Eight pollers, each on its own connection, claim the same order at once: one wins.
	stores := make([]CloudOrderStore, 8)
	for i := range stores {
		stores[i] = open()
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(store CloudOrderStore, i int) {
			defer wg.Done()
			claimed, err := store.Claim(ctx, paid, fmt.Sprintf("%s-%d", claimant, i%2))
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if claimed {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(stores[i], i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d pollers claimed one order", wins)
	}
	var winner string
	if err := db.QueryRow(ctx, `SELECT claimed_by FROM cloud_orders WHERE id = $1 AND state = 'provisioning' AND claimed_at IS NOT NULL`, paid).Scan(&winner); err != nil {
		t.Fatal(err)
	}
	loser := claimant + "-0"
	if winner == loser {
		loser = claimant + "-1"
	}

	// A restart is a new connection: the claim is listed for its holder, and cannot be taken again.
	restarted := open()
	held, err := restarted.ListOrders(ctx, winner)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 || held[0].State != cloudOrderProvisioning || held[0].ClaimedBy != winner {
		t.Fatalf("after a restart the holder lists %+v", held)
	}
	if again, err := restarted.Claim(ctx, paid, winner); err != nil || again {
		t.Fatalf("a held order was claimed again: %v %v", again, err)
	}
	if other, err := open().ListOrders(ctx, loser); err != nil || len(other) != 1 || other[0].ID != second {
		t.Fatalf("another claimant lists %+v (%v), want only the other paid order", other, err)
	}

	// Only the holder moves it on, and only once.
	if moved, err := restarted.MarkReady(ctx, paid, loser, "x.lakelive.example"); err != nil || moved {
		t.Fatalf("a non-holder moved the order: %v %v", moved, err)
	}
	if moved, err := restarted.MarkReady(ctx, paid, winner, "order-0a1b2c3d4e5f.lakelive.example"); err != nil || !moved {
		t.Fatalf("the holder could not mark it ready: %v %v", moved, err)
	}
	if moved, err := restarted.MarkFailed(ctx, paid, winner, "late"); err != nil || moved {
		t.Fatalf("a ready order was moved again: %v %v", moved, err)
	}
	if claimed, err := restarted.Claim(ctx, second, claimant); err != nil || !claimed {
		t.Fatalf("claim the second: %v %v", claimed, err)
	}
	if moved, err := restarted.MarkFailed(ctx, second, claimant, "dedicated clusters are created on Azure only today"); err != nil || !moved {
		t.Fatalf("mark failed: %v %v", moved, err)
	}

	var state, host, reason, subscription, email string
	if err := db.QueryRow(ctx, `SELECT state, COALESCE(host, ''), COALESCE(failure_reason, ''), stripe_subscription_id, contact_email FROM cloud_orders WHERE id = $1`, paid).
		Scan(&state, &host, &reason, &subscription, &email); err != nil {
		t.Fatal(err)
	}
	if state != cloudOrderReady || host != "order-0a1b2c3d4e5f.lakelive.example" || reason != "" || subscription != "sub_a" || email != "a@example.com" {
		t.Fatalf("ready row: %s %s %q %s %s", state, host, reason, subscription, email)
	}
	if err := db.QueryRow(ctx, `SELECT state, COALESCE(host, ''), failure_reason, stripe_subscription_id FROM cloud_orders WHERE id = $1`, second).
		Scan(&state, &host, &reason, &subscription); err != nil {
		t.Fatal(err)
	}
	if state != cloudOrderFailed || host != "" || reason != "dedicated clusters are created on Azure only today" || subscription != "sub_b" {
		t.Fatalf("failed row: %s %q %q %s", state, host, reason, subscription)
	}
	var untouched string
	if err := db.QueryRow(ctx, `SELECT state FROM cloud_orders WHERE id = $1 AND claimed_by IS NULL`, submitted).Scan(&untouched); err != nil || untouched != "submitted" {
		t.Fatalf("the order form was touched: %q %v", untouched, err)
	}

	// One event per change, every one the operator's.
	rows, err := db.Query(ctx, `SELECT order_id::text, COALESCE(from_state, ''), to_state, actor FROM cloud_order_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var id, from, to, actor string
		if err := rows.Scan(&id, &from, &to, &actor); err != nil {
			t.Fatal(err)
		}
		events = append(events, fmt.Sprintf("%s %s->%s %s", id[:8], from, to, actor))
	}
	want := []string{
		"0a1b2c3d paid->provisioning operator",
		"0a1b2c3d provisioning->ready operator",
		"1b2c3d4e paid->provisioning operator",
		"1b2c3d4e provisioning->failed operator",
	}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}
