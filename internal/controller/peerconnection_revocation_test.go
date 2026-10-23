/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func newRunningPeer(t *testing.T) (*peerCluster, corev1.Secret) {
	t.Helper()
	ctx := context.Background()
	ca, err := newTunnelCA("pair", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := issueTunnelLeaf(ca, tunnelLeafRequest{DNSNames: []string{"eu.peer.orama.internal"}, Server: true, Client: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pair-credentials", Namespace: "peer-test"}, Data: map[string][]byte{
		"tls.crt": cert, "tls.key": key, "ca.crt": ca.CertPEM, "peer-name": []byte("us.peer.orama.internal"),
	}}
	cluster := newPeerCluster(t, newLakeScheme(t), secret.Namespace, &oramav1alpha1.PeerConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "pair", Namespace: secret.Namespace, UID: "peer-uid"},
		Spec: oramav1alpha1.PeerConnectionSpec{PeerID: testPeerID, Desired: "present", Remote: "20.0.0.1:7446", Generation: 1,
			Imports: []oramav1alpha1.PeerImport{{Name: "writer.eu", Port: 5432}}},
	}, &secret)
	cluster.reconcile(t)
	cluster.publish(t, "40.0.0.2")
	cluster.available(t)
	if peer := cluster.reconcile(t); peer.Status.Phase != oramav1alpha1.PeerPhaseReady {
		t.Fatalf("initial peer = %+v", peer.Status)
	}
	if err := cluster.kube.Get(ctx, client.ObjectKeyFromObject(&secret), &secret); err != nil {
		t.Fatal(err)
	}
	return cluster, secret
}

func assertPeerWithdrawn(t *testing.T, cluster *peerCluster) {
	t.Helper()
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.ConfigMap{}, &corev1.Service{}} {
		if err := cluster.kube.Get(context.Background(), client.ObjectKey{Namespace: cluster.ns, Name: cluster.name + "-gateway"}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("%T survived credential withdrawal: %v", object, err)
		}
	}
	if err := cluster.kube.Get(context.Background(), client.ObjectKey{Namespace: cluster.ns, Name: "peer-writer-eu"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("import Service survived credential withdrawal: %v", err)
	}
	// Policies belong to the sandbox; Kubernetes GC removes them with a deleted sandbox.
	if err := cluster.kube.Get(context.Background(), client.ObjectKey{Namespace: cluster.ns, Name: networkSandboxName}, &oramav1alpha1.NetworkSandbox{}); apierrors.IsNotFound(err) {
		return
	}
	var policies networkingv1.NetworkPolicyList
	if err := cluster.kube.List(context.Background(), &policies, client.InNamespace(cluster.ns)); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies.Items {
		if strings.HasPrefix(policy.Name, "peer-pair-") {
			t.Fatalf("policy %s survived credential withdrawal", policy.Name)
		}
	}
}

func TestPeerCredentialsWithdrawalRevokesRunningGateway(t *testing.T) {
	for _, change := range []string{"deleted", "stale-cache", "deleting", "tls.crt", "tls.key", "ca.crt", "peer-name", "bad-cert", "bad-key", "bad-ca", "blank-peer-name", "missing-sandbox", "secret-reference"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			cluster, secret := newRunningPeer(t)
			var peer oramav1alpha1.PeerConnection
			if err := cluster.kube.Get(ctx, client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}, &peer); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "deleted", "missing-sandbox", "stale-cache":
				if err := cluster.kube.Delete(ctx, &secret); err != nil {
					t.Fatal(err)
				}
				if change == "stale-cache" {
					cluster.r.APIReader = cluster.kube
					cluster.r.Client = interceptor.NewClient(cluster.kube, interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							if cached, ok := object.(*corev1.Secret); ok && key == client.ObjectKeyFromObject(&secret) {
								secret.DeepCopyInto(cached)
								return nil
							}
							return c.Get(ctx, key, object, opts...)
						},
					})
				}
				if change == "missing-sandbox" {
					if err := cluster.kube.Delete(ctx, &oramav1alpha1.NetworkSandbox{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.ns, Name: networkSandboxName}}); err != nil {
						t.Fatal(err)
					}
				}
			case "deleting":
				secret.Finalizers = []string{"test.orama.cloud/hold"}
				if err := cluster.kube.Update(ctx, &secret); err != nil {
					t.Fatal(err)
				}
				if err := cluster.kube.Delete(ctx, &secret); err != nil {
					t.Fatal(err)
				}
			case "secret-reference":
				peer.Spec.CredentialsSecretName = "missing"
				if err := cluster.kube.Update(ctx, &peer); err != nil {
					t.Fatal(err)
				}
			default:
				switch change {
				case "bad-cert":
					secret.Data["tls.crt"] = []byte("invalid")
				case "bad-key":
					secret.Data["tls.key"] = []byte("invalid")
				case "bad-ca":
					secret.Data["ca.crt"] = []byte("invalid")
				case "blank-peer-name":
					secret.Data["peer-name"] = []byte(" \n")
				default:
					delete(secret.Data, change)
				}
				if err := cluster.kube.Update(ctx, &secret); err != nil {
					t.Fatal(err)
				}
			}
			peer = cluster.reconcile(t)
			if peer.Status.Phase == oramav1alpha1.PeerPhaseReady || len(peer.Status.Imports) != 0 {
				t.Fatalf("withdrawn credentials still ready: %+v", peer.Status)
			}
			assertPeerWithdrawn(t, cluster)
		})
	}
}

func TestPeerCredentialRotationWaitsForOldGatewayTermination(t *testing.T) {
	for _, change := range []string{"tls.crt", "tls.key", "ca.crt", "peer-name", "secret-reference", "secret-recreated"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			cluster, secret := newRunningPeer(t)
			gatewayKey := client.ObjectKey{Namespace: cluster.ns, Name: cluster.name + "-gateway"}
			var old appsv1.Deployment
			if err := cluster.kube.Get(ctx, gatewayKey, &old); err != nil {
				t.Fatal(err)
			}
			// Hold deletion to model foreground GC waiting for the old ReplicaSets/pods.
			old.Finalizers = []string{"test.orama.cloud/old-pods"}
			if err := cluster.kube.Update(ctx, &old); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "secret-reference", "secret-recreated":
				if change == "secret-reference" {
					secret.Name = "replacement"
				} else if err := cluster.kube.Delete(ctx, &secret); err != nil {
					t.Fatal(err)
				}
				secret.ResourceVersion = ""
				secret.UID = "replacement-uid"
				if err := cluster.kube.Create(ctx, &secret); err != nil {
					t.Fatal(err)
				}
				if change == "secret-reference" {
					var peer oramav1alpha1.PeerConnection
					if err := cluster.kube.Get(ctx, client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}, &peer); err != nil {
						t.Fatal(err)
					}
					peer.Spec.CredentialsSecretName = secret.Name
					if err := cluster.kube.Update(ctx, &peer); err != nil {
						t.Fatal(err)
					}
				}
			default:
				// A PEM whitespace change is still valid and must change the trust fingerprint.
				secret.Data[change] = append(secret.Data[change], '\n')
				if err := cluster.kube.Update(ctx, &secret); err != nil {
					t.Fatal(err)
				}
			}
			foreground := false
			cluster.r.Client = interceptor.NewClient(cluster.kube, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					if _, ok := object.(*appsv1.Deployment); ok {
						options := &client.DeleteOptions{}
						options.ApplyOptions(opts)
						foreground = options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			peer := cluster.reconcile(t)
			if !foreground || peer.Status.Phase != oramav1alpha1.PeerPhasePending || !strings.Contains(peer.Status.Message, "terminate") {
				t.Fatalf("rotation did not wait for foreground termination: foreground=%t, status=%+v", foreground, peer.Status)
			}
			if err := cluster.kube.Get(ctx, gatewayKey, &corev1.Service{}); err != nil {
				t.Fatalf("rotation lost the public Service: %v", err)
			}
			var terminating appsv1.Deployment
			if err := cluster.kube.Get(ctx, gatewayKey, &terminating); err != nil {
				t.Fatal(err)
			}
			if terminating.DeletionTimestamp.IsZero() || terminating.Spec.Template.Annotations["orama.cloud/peer-credentials"] != old.Spec.Template.Annotations["orama.cloud/peer-credentials"] {
				t.Fatal("old gateway was updated or replaced before termination")
			}
			// Once the old pods are gone, reconciliation may create a fresh TLS process.
			terminating.Finalizers = nil
			if err := cluster.kube.Update(ctx, &terminating); err != nil {
				t.Fatal(err)
			}
			cluster.reconcile(t)
			var replacement appsv1.Deployment
			if err := cluster.kube.Get(ctx, gatewayKey, &replacement); err != nil {
				t.Fatal(err)
			}
			if replacement.Spec.Template.Annotations["orama.cloud/peer-credentials"] == old.Spec.Template.Annotations["orama.cloud/peer-credentials"] {
				t.Fatal("replacement gateway still uses old credentials")
			}
		})
	}
}

func TestPeerCredentialWithdrawalContinuesAfterPolicyCleanupError(t *testing.T) {
	ctx := context.Background()
	cluster, secret := newRunningPeer(t)
	if err := cluster.kube.Delete(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	cluster.r.Client = interceptor.NewClient(cluster.kube, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if _, ok := object.(*oramav1alpha1.NetworkSandbox); ok {
				return fmt.Errorf("sandbox update unavailable")
			}
			return c.Update(ctx, object, opts...)
		},
	})
	_, err := cluster.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}})
	if err == nil || !strings.Contains(err.Error(), "sandbox update unavailable") {
		t.Fatalf("cleanup error = %v", err)
	}
	if err := cluster.kube.Get(ctx, client.ObjectKey{Namespace: cluster.ns, Name: cluster.name + "-gateway"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("policy cleanup error left an authorized gateway: %v", err)
	}
}

func TestPeerCredentialReadFailureWithdrawsGateway(t *testing.T) {
	cluster, _ := newRunningPeer(t)
	cluster.r.APIReader = interceptor.NewClient(cluster.kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, ok := object.(*corev1.Secret); ok {
				return fmt.Errorf("credentials unavailable")
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	_, err := cluster.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}})
	if err == nil || !strings.Contains(err.Error(), "credentials unavailable") {
		t.Fatalf("credential read error = %v", err)
	}
	assertPeerWithdrawn(t, cluster)
	var peer oramav1alpha1.PeerConnection
	if err := cluster.kube.Get(context.Background(), client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}, &peer); err != nil {
		t.Fatal(err)
	}
	if peer.Status.Phase != oramav1alpha1.PeerPhaseFailed {
		t.Fatalf("credential read failure left stale readiness: %+v", peer.Status)
	}
}

func TestPeerCredentialsReturningCannotCancelGatewayWithdrawal(t *testing.T) {
	ctx := context.Background()
	cluster, secret := newRunningPeer(t)
	gatewayKey := client.ObjectKey{Namespace: cluster.ns, Name: cluster.name + "-gateway"}
	var deployment appsv1.Deployment
	if err := cluster.kube.Get(ctx, gatewayKey, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Finalizers = []string{"test.orama.cloud/old-pods"}
	if err := cluster.kube.Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if err := cluster.kube.Delete(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	cluster.reconcile(t)
	secret.ResourceVersion = ""
	if err := cluster.kube.Create(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	peer := cluster.reconcile(t)
	if peer.Status.Phase != oramav1alpha1.PeerPhasePending || !strings.Contains(peer.Status.Message, "terminate") {
		t.Fatalf("returning credentials bypassed withdrawal: %+v", peer.Status)
	}
	if err := cluster.kube.Get(ctx, gatewayKey, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.DeletionTimestamp.IsZero() {
		t.Fatal("returning credentials revived the old gateway")
	}
	if err := cluster.kube.Get(ctx, gatewayKey, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("projection was restored while old pods were terminating: %v", err)
	}
}

func TestPeerCredentialsFingerprintSeparatesCertificateAndTrust(t *testing.T) {
	ctx := context.Background()
	cluster, secret := newRunningPeer(t)
	var peer oramav1alpha1.PeerConnection
	if err := cluster.kube.Get(ctx, client.ObjectKey{Namespace: cluster.ns, Name: cluster.name}, &peer); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := newTunnelCA("withdrawn-root", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certificate := append([]byte(nil), secret.Data["tls.crt"]...)
	trust := append([]byte("\n"), secret.Data["ca.crt"]...)
	root := append([]byte("\n"), withdrawn.CertPEM...)
	// Moving the extra root from CA trust to the certificate chain preserves a
	// NUL-delimited concatenation, but must revoke the old gateway's trust in that root.
	secret.Data["ca.crt"] = append(append(append([]byte(nil), root...), 0), trust...)
	if err := cluster.kube.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	before, err := cluster.r.credentials(ctx, &peer)
	if err != nil {
		t.Fatalf("initial PEM bundle: %v", err)
	}
	secret.Data["tls.crt"] = append(append(append([]byte(nil), certificate...), 0), root...)
	secret.Data["ca.crt"] = trust
	if err := cluster.kube.Update(ctx, &secret); err != nil {
		t.Fatal(err)
	}
	after, err := cluster.r.credentials(ctx, &peer)
	if err != nil {
		t.Fatalf("rotated PEM bundle: %v", err)
	}
	if before.Hash == after.Hash {
		t.Fatal("removing a trusted CA by moving it between PEM fields preserved the fingerprint")
	}
}
