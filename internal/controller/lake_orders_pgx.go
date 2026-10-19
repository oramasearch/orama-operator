/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The pgx half of the order provisioning (lake_orders.go, operator#634): lake's `cloud_orders`
// table (lake#831), read and written on a member's control-plane Postgres. The operator reads
// `paid` rows and its own claims, and writes only `state`, `host`, `failure_reason`,
// `claimed_by` and `claimed_at`, plus one `cloud_order_events` row per change, with actor
// `operator`. Every state change is one statement, so the row and its event land together.

// errCloudOrdersTableMissing is returned while lake's schema has no cloud_orders table yet.
var errCloudOrdersTableMissing = errors.New("lake's control plane has no cloud_orders table yet (lake#831's migration)")

const (
	cloudOrdersTableExistsSQL = `SELECT to_regclass('cloud_orders') IS NOT NULL AND to_regclass('cloud_order_events') IS NOT NULL`

	// listCloudOrdersSQL is every paid order, and every order this claimant holds that is still
	// its concern: provisioning (resumed after a restart), ready (its resources are kept as
	// described) and failed (counted).
	listCloudOrdersSQL = `SELECT id::text, offer, COALESCE(cloud_provider, ''), COALESCE(region, ''), state,
       COALESCE(host, ''), COALESCE(claimed_by, '')
  FROM cloud_orders
 WHERE state = 'paid'
    OR (claimed_by = $1 AND state IN ('provisioning', 'ready', 'failed'))
 ORDER BY id`

	// claimCloudOrderSQL moves one paid order to provisioning for one claimant. The WHERE on
	// state makes it exactly once: of two pollers, or a poller and its own restart, only one
	// statement finds the row still paid.
	claimCloudOrderSQL = `WITH claimed AS (
  UPDATE cloud_orders SET state = 'provisioning', claimed_by = $2, claimed_at = now()
   WHERE id = $1::uuid AND state = 'paid'
  RETURNING id)
INSERT INTO cloud_order_events (order_id, at, from_state, to_state, actor, detail)
SELECT id, now(), 'paid', 'provisioning', 'operator', $3 FROM claimed
RETURNING order_id`

	// finishCloudOrderSQL moves an order this claimant holds from provisioning to ready or
	// failed. host and failure_reason are written only when given.
	finishCloudOrderSQL = `WITH moved AS (
  UPDATE cloud_orders SET state = $3, host = COALESCE($4::text, host), failure_reason = COALESCE($5::text, failure_reason)
   WHERE id = $1::uuid AND state = 'provisioning' AND claimed_by = $2
  RETURNING id)
INSERT INTO cloud_order_events (order_id, at, from_state, to_state, actor, detail)
SELECT id, now(), 'provisioning', $3, 'operator', $6 FROM moved
RETURNING order_id`
)

// CloudOrder is one row of lake's cloud_orders, as far as the operator reads it.
type CloudOrder struct {
	ID            string
	Offer         string
	CloudProvider string
	Region        string
	State         string
	Host          string
	ClaimedBy     string
}

// CloudOrderStore reads and writes one control plane's orders. The pgx implementation talks to
// lake's control Postgres; tests substitute an in-memory fake.
type CloudOrderStore interface {
	// ListOrders returns every paid order and the orders claimant holds. It returns
	// errCloudOrdersTableMissing while lake's schema has no cloud_orders table.
	ListOrders(ctx context.Context, claimant string) ([]CloudOrder, error)
	// Claim moves a paid order to provisioning for claimant. False means another claimant, or
	// an earlier pass, has it.
	Claim(ctx context.Context, id, claimant string) (bool, error)
	// MarkReady and MarkFailed move an order claimant holds from provisioning. False means it
	// is no longer provisioning, or not claimant's.
	MarkReady(ctx context.Context, id, claimant, host string) (bool, error)
	MarkFailed(ctx context.Context, id, claimant, reason string) (bool, error)
	Close(ctx context.Context) error
}

// orderSourceMember is one LakeSet member whose control plane holds orders.
type orderSourceMember struct {
	ClusterRef string
	Name       string // the Lake's name
	Namespace  string // the Lake's namespace on its cluster
}

// CloudOrderStoreOpener connects to a member's control Postgres. c is the client of the cluster
// the operator runs on, which holds the member's cluster's kubeconfig.
type CloudOrderStoreOpener func(ctx context.Context, c client.Client, member orderSourceMember) (CloudOrderStore, error)

type pgxCloudOrderStore struct {
	conn    *pgx.Conn
	release func()
}

func (s *pgxCloudOrderStore) ListOrders(ctx context.Context, claimant string) ([]CloudOrder, error) {
	var exists bool
	if err := s.conn.QueryRow(ctx, cloudOrdersTableExistsSQL).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errCloudOrdersTableMissing
	}
	rows, err := s.conn.Query(ctx, listCloudOrdersSQL, claimant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []CloudOrder
	for rows.Next() {
		var o CloudOrder
		if err := rows.Scan(&o.ID, &o.Offer, &o.CloudProvider, &o.Region, &o.State, &o.Host, &o.ClaimedBy); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func (s *pgxCloudOrderStore) Claim(ctx context.Context, id, claimant string) (bool, error) {
	return oneRow(s.conn.Query(ctx, claimCloudOrderSQL, id, claimant, "claimed by "+claimant))
}

func (s *pgxCloudOrderStore) MarkReady(ctx context.Context, id, claimant, host string) (bool, error) {
	return oneRow(s.conn.Query(ctx, finishCloudOrderSQL, id, claimant, cloudOrderReady, host, nil, "ready at "+host))
}

func (s *pgxCloudOrderStore) MarkFailed(ctx context.Context, id, claimant, reason string) (bool, error) {
	return oneRow(s.conn.Query(ctx, finishCloudOrderSQL, id, claimant, cloudOrderFailed, nil, reason, reason))
}

func (s *pgxCloudOrderStore) Close(ctx context.Context) error {
	err := s.conn.Close(ctx)
	if s.release != nil {
		s.release()
	}
	return err
}

// oneRow reports whether a statement returned a row.
func oneRow(rows pgx.Rows, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	rows.Close()
	return found, rows.Err()
}

// openPgxCloudOrderStore connects with a control DB uri, through forward when it is not nil.
func openPgxCloudOrderStore(ctx context.Context, uri string, forward *forwardedPort) (CloudOrderStore, error) {
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		if forward != nil {
			forward.stop()
		}
		return nil, fmt.Errorf("the control DB uri cannot be parsed: %w", err)
	}
	config.ConnectTimeout = serviceStoreConnectTimeout
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["application_name"] = "orama-operator-orders"
	release := func() {}
	if forward != nil {
		// The uri names the member's in-cluster Service. The connection goes to the local end
		// of the port-forward instead; TLS, when the uri asks for it, still checks the Service's
		// name, which is the name on the server's certificate.
		redirect := func(host *string, port *uint16, tlsName func(string)) {
			tlsName(*host)
			*host, *port = "127.0.0.1", forward.local
		}
		redirect(&config.Host, &config.Port, func(name string) {
			if config.TLSConfig != nil {
				config.TLSConfig.ServerName = name
			}
		})
		for _, fallback := range config.Fallbacks {
			fb := fallback
			redirect(&fb.Host, &fb.Port, func(name string) {
				if fb.TLSConfig != nil {
					fb.TLSConfig.ServerName = name
				}
			})
		}
		release = forward.stop
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		release()
		return nil, err
	}
	return &pgxCloudOrderStore{conn: conn, release: release}, nil
}

// forwardedPort is a port-forward to a pod, open until stop is called.
type forwardedPort struct {
	local uint16
	stop  func()
}

// openCloudOrderStoreThroughCluster is the production CloudOrderStoreOpener. The member's Lake
// runs on a managed cluster, and its control Postgres has no address outside it. The operator
// already holds that cluster's admin kubeconfig (its KubernetesCluster's), so it reads the
// Lake's control DB app Secret there and opens a port-forward through the cluster's API server to
// the Postgres primary: the same path `kubectl port-forward` takes, with no Service, Ingress or
// firewall rule added for it.
func openCloudOrderStoreThroughCluster(ctx context.Context, c client.Client, member orderSourceMember) (CloudOrderStore, error) {
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, c, member.ClusterRef)
	if err != nil {
		return nil, err
	}
	if waiting != "" {
		return nil, errors.New(waiting)
	}
	getter, leaf, err := leafClients(kubeconfig, member.Namespace)
	if err != nil {
		return nil, err
	}
	secretName := lakeControlDB(member.Name) + "-app"
	var app corev1.Secret
	if err := leaf.Get(ctx, client.ObjectKey{Namespace: member.Namespace, Name: secretName}, &app); err != nil {
		return nil, fmt.Errorf("the control DB Secret %s/%s cannot be read on %s: %w", member.Namespace, secretName, member.ClusterRef, err)
	}
	uri := string(app.Data["uri"])
	if uri == "" {
		return nil, fmt.Errorf("the control DB Secret %s/%s on %s has no uri yet", member.Namespace, secretName, member.ClusterRef)
	}
	pod, err := controlDBPrimary(ctx, leaf, member)
	if err != nil {
		return nil, err
	}
	restConfig, err := getter.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	forward, err := portForward(ctx, restConfig, member.Namespace, pod, 5432)
	if err != nil {
		return nil, fmt.Errorf("cannot forward to %s/%s on %s: %w", member.Namespace, pod, member.ClusterRef, err)
	}
	return openPgxCloudOrderStore(ctx, uri, forward)
}

// controlDBPrimary is the running primary pod of a Lake's CNPG cluster.
func controlDBPrimary(ctx context.Context, c client.Client, member orderSourceMember) (string, error) {
	cluster := lakeControlDB(member.Name)
	for _, role := range []map[string]string{{"cnpg.io/instanceRole": "primary"}, {"role": "primary"}} {
		labels := client.MatchingLabels{cnpgClusterLabel: cluster}
		for k, v := range role {
			labels[k] = v
		}
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(member.Namespace), labels); err != nil {
			return "", fmt.Errorf("the control DB pods cannot be listed on %s: %w", member.ClusterRef, err)
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
				return pod.Name, nil
			}
		}
	}
	return "", fmt.Errorf("the control DB %s/%s on %s has no running primary", member.Namespace, cluster, member.ClusterRef)
}

// portForward opens a port-forward to one pod port on 127.0.0.1 and an unused local port.
func portForward(ctx context.Context, rc *rest.Config, namespace, pod string, port int) (*forwardedPort, error) {
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(rc)
	if err != nil {
		return nil, err
	}
	url := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(port)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	stop := func() { once.Do(func() { close(stopCh) }) }
	failed := make(chan error, 1)
	go func() { failed <- forwarder.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-failed:
		stop()
		return nil, err
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	case <-time.After(serviceStoreConnectTimeout):
		stop()
		return nil, errors.New("the port-forward did not open")
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) == 0 {
		stop()
		return nil, fmt.Errorf("the port-forward has no local port: %v", err)
	}
	return &forwardedPort{local: ports[0].Local, stop: stop}, nil
}
