/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Cross-cluster read (operator#396). A primary with spec.remoteReaders publishes its replication
// stream and control database to read members in other clusters, over public load balancers that
// accept only the read members' egress IPs:
//
//   - the broker gets a TLS stream listener (:5551) with a server certificate from an
//     operator-minted CA, exposed by <cr>-broker-remote; plain :5552 stays in-cluster;
//   - the CNPG primary instance is exposed by <cr>-controldb-remote (:5432), where a replica
//     authenticates as streaming_replica with CNPG's client certificate;
//   - a read-only broker user, oramacore-remote-reader, is declared by its own RabbitMQTopology,
//     so removing it removes the user from the broker;
//   - <cr>-remote-read holds everything a read member needs, and status.remoteRead says where to
//     connect once all of it is in place.
//
// A read member whose spec.peers names that endpoint and a copy of the Secret runs its readers
// against the primary's stream and, with controlDB.replicate, a CNPG replica of its control
// database. An OramaCloudV1Set writes both sides; see "Cross-cluster read" in docs/crd-reference.md.

const (
	remoteReadComponent = "remote-read"
	peerComponent       = "peer"

	// brokerStreamTLSPort is the broker's TLS stream listener, the only port a remote reader reaches.
	brokerStreamTLSPort = 5551
	brokerStreamPort    = 5552
	controlDBPort       = 5432

	// remoteReaderBrokerUser is the broker account of every remote reader.
	remoteReaderBrokerUser = "oramacore-remote-reader"

	// Keys of the remote-read credentials Secret.
	remoteReadBrokerCAKey       = "broker-ca.crt"
	remoteReadBrokerUserKey     = "broker-username"
	remoteReadBrokerPasswordKey = "broker-password"
	remoteReadControlDBCAKey    = "controldb-ca.crt"
	remoteReadControlDBCertKey  = "controldb-replication.crt"
	remoteReadControlDBKeyKey   = "controldb-replication.key"

	// Where the broker reads its TLS files, and where a remote reader finds the broker's CA.
	brokerTLSDir            = "/etc/rabbitmq/tls"
	readerBrokerCADir       = "/etc/oramacore-broker"
	readerBrokerCAPath      = readerBrokerCADir + "/ca.crt"
	brokerTLSCertAnnotation = "orama.cloud/stream-tls-cert"

	// The broker CA lives ten years and is never rotated by the operator. The server certificate
	// lives a year and is reissued a month before it expires, or when its addresses change.
	brokerCAValidity         = 10 * 365 * 24 * time.Hour
	brokerServerCertValidity = 365 * 24 * time.Hour
	brokerServerCertRenewal  = 30 * 24 * time.Hour

	// remoteReadResync re-reads a published primary's sources (CNPG rotates its replication
	// certificate on its own schedule) so the credentials Secret follows them.
	remoteReadResync = 10 * time.Minute

	remoteReadConditionType = "RemoteReadPublished"
	peerConditionType       = "PeerReady"
)

func remoteReadSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-remote-read" }
func brokerTLSSecretName(oc *oramav1alpha1.OramaCloudV1) string  { return oc.Name + "-broker-tls" }
func brokerRemoteServiceName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-broker-remote"
}
func controlDBRemoteServiceName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-remote"
}
func remoteReaderPasswordSecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-broker-remote-reader"
}
func remoteReadTopologyName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-broker-remote-read"
}

// remoteReadActive reports whether this CR publishes its stream to other clusters: a primary with
// at least one allowed source range.
func remoteReadActive(oc *oramav1alpha1.OramaCloudV1) bool {
	return oc.Spec.Region.Role == "primary" && oc.Spec.RemoteReaders != nil && len(oc.Spec.RemoteReaders.AllowedSourceRanges) > 0
}

// remotePeer is the primary a read member reads from: the first peer naming a stream endpoint and a
// credentials Secret. nil on a primary, and on a read member with no such peer.
func remotePeer(oc *oramav1alpha1.OramaCloudV1) *oramav1alpha1.PeerSpec {
	if oc.Spec.Region.Role != "read" {
		return nil
	}
	for i := range oc.Spec.Peers {
		p := &oc.Spec.Peers[i]
		if p.BrokerStreamEndpoint != "" && p.CredentialsSecretRef != nil && p.CredentialsSecretRef.Name != "" {
			return p
		}
	}
	return nil
}

// reconcileRemoteRead owns everything a primary publishes to remote readers, and removes all of it
// when spec.remoteReaders is unset or empty (disable is not leave running).
func (r *OramaCloudV1Reconciler) reconcileRemoteRead(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !remoteReadActive(oc) {
		return r.teardownRemoteRead(ctx, oc)
	}
	if !storeInCluster(oc, oc.Spec.Components.Broker) {
		return fmt.Errorf("spec.remoteReaders publishes the broker's stream, so the broker must run in this cluster (spec.components.broker enabled, in-cluster, placement local)")
	}
	ranges := slices.Sorted(slices.Values(oc.Spec.RemoteReaders.AllowedSourceRanges))
	ranges = slices.Compact(ranges)

	brokerSvc, err := r.upsertRemoteService(ctx, oc, brokerRemoteServiceName(oc), labelsFor(oc, "broker"), "stream-tls", brokerStreamTLSPort, ranges)
	if err != nil {
		return fmt.Errorf("broker remote service: %w", err)
	}
	if err := r.reconcileBrokerTLS(ctx, oc, loadBalancerAddress(brokerSvc)); err != nil {
		return fmt.Errorf("broker TLS: %w", err)
	}
	if err := r.reconcileRemoteReaderUser(ctx, oc); err != nil {
		return fmt.Errorf("remote reader broker user: %w", err)
	}
	dbSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: controlDBRemoteServiceName(oc), Namespace: oc.Namespace}}
	if controlDBInCluster(oc) {
		// The CNPG primary instance, whichever pod holds it after a failover. CNPG labels it.
		selector := map[string]string{"cnpg.io/cluster": controlDBClusterName(oc), "cnpg.io/instanceRole": "primary"}
		if _, err := r.upsertRemoteService(ctx, oc, dbSvc.Name, selector, "postgres", controlDBPort, ranges); err != nil {
			return fmt.Errorf("control DB remote service: %w", err)
		}
	} else if err := r.deleteCloudV1Owned(ctx, oc, dbSvc); err != nil {
		return err
	}
	return r.reconcileRemoteReadSecret(ctx, oc)
}

// teardownRemoteRead removes the remote reader's broker user (through its topology's finalizer),
// then the remote Services and Secrets.
func (r *OramaCloudV1Reconciler) teardownRemoteRead(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	topo := &oramav1alpha1.RabbitMQTopology{ObjectMeta: metav1.ObjectMeta{Name: remoteReadTopologyName(oc), Namespace: oc.Namespace}}
	if err := r.deleteCloudV1Owned(ctx, oc, topo); err != nil {
		return err
	}
	return r.teardownByComponent(ctx, oc, remoteReadComponent)
}

// upsertRemoteService is a LoadBalancer Service that accepts only the allowed source ranges.
// externalTrafficPolicy Local keeps the client's address and skips a hop between nodes.
func (r *OramaCloudV1Reconciler) upsertRemoteService(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string, selector map[string]string, portName string, port int32, ranges []string) (*corev1.Service, error) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		// Keep the node port the API server allocated, so an unchanged Service is not updated.
		var nodePort int32
		for _, p := range svc.Spec.Ports {
			if p.Name == portName {
				nodePort = p.NodePort
			}
		}
		svc.Labels = labelsFor(oc, remoteReadComponent)
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{{Name: portName, Protocol: corev1.ProtocolTCP, Port: port, TargetPort: intstr.FromInt32(port), NodePort: nodePort}}
		svc.Spec.LoadBalancerSourceRanges = ranges
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
		return controllerutil.SetControllerReference(oc, svc, r.Scheme)
	})
	return svc, err
}

// loadBalancerAddress is the address the cloud gave a LoadBalancer Service, or "" before it has one.
func loadBalancerAddress(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
	}
	return ""
}

// reconcileBrokerTLS keeps <cr>-broker-tls: a CA minted once (ca.crt, ca.key) and a server
// certificate (tls.crt, tls.key) for the broker's in-cluster names and the remote Service's
// address. The server certificate is issued only once the address is known, because a remote
// reader verifies it against the address it connects to.
func (r *OramaCloudV1Reconciler) reconcileBrokerTLS(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, address string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: brokerTLSSecretName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = labelsFor(oc, remoteReadComponent)
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		now := r.now()
		ca, caKey, err := parseCA(sec.Data["ca.crt"], sec.Data["ca.key"])
		if err != nil {
			caPEM, keyPEM, err := mintBrokerCA(oc, now)
			if err != nil {
				return err
			}
			sec.Data = map[string][]byte{"ca.crt": caPEM, "ca.key": keyPEM}
			if ca, caKey, err = parseCA(caPEM, keyPEM); err != nil {
				return err
			}
		}
		if address != "" && !serverCertCovers(sec.Data["tls.crt"], ca, brokerServerNames(oc), address, now) {
			certPEM, keyPEM, err := issueBrokerServerCert(ca, caKey, brokerServerNames(oc), address, now)
			if err != nil {
				return err
			}
			sec.Data["tls.crt"], sec.Data["tls.key"] = certPEM, keyPEM
		}
		return controllerutil.SetControllerReference(oc, sec, r.Scheme)
	})
	return err
}

// brokerServerNames are the in-cluster names the broker's server certificate also carries.
func brokerServerNames(oc *oramav1alpha1.OramaCloudV1) []string {
	svc := oc.Name + "-broker"
	return []string{svc, svc + "." + oc.Namespace + ".svc", svc + "." + oc.Namespace + ".svc.cluster.local"}
}

// brokerStreamTLS returns the hash of the broker's TLS material when the broker should serve its
// TLS stream listener, or "" when it should not: remote readers are off, or no server certificate
// has been issued yet. The hash is a pod-template annotation, so new material restarts the broker.
func (r *OramaCloudV1Reconciler) brokerStreamTLS(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	if !remoteReadActive(oc) {
		return "", nil
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: brokerTLSSecretName(oc)}, sec); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if len(sec.Data["tls.crt"]) == 0 || len(sec.Data["tls.key"]) == 0 {
		return "", nil
	}
	return tlsMaterialHash(sec), nil
}

func tlsMaterialHash(sec *corev1.Secret) string {
	sum := sha256.Sum256(bytes.Join([][]byte{sec.Data["ca.crt"], sec.Data["tls.crt"], sec.Data["tls.key"]}, []byte{0}))
	return fmt.Sprintf("%x", sum[:16])
}

// brokerTLSVolume mounts the CA and the server certificate and key, never the CA's key.
func brokerTLSVolume(oc *oramav1alpha1.OramaCloudV1) (corev1.Volume, []corev1.VolumeMount) {
	files := []string{"ca.crt", "tls.crt", "tls.key"}
	items := make([]corev1.KeyToPath, 0, len(files))
	mounts := make([]corev1.VolumeMount, 0, len(files))
	for _, f := range files {
		items = append(items, corev1.KeyToPath{Key: f, Path: f})
		mounts = append(mounts, corev1.VolumeMount{Name: "stream-tls", MountPath: brokerTLSDir + "/" + f, SubPath: f, ReadOnly: true})
	}
	return corev1.Volume{Name: "stream-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: brokerTLSSecretName(oc), Items: items}}}, mounts
}

// reconcileRemoteReaderUser declares the remote readers' broker account in its own RabbitMQTopology,
// so deleting that topology deletes the account from the broker. The password is generated once.
//
// OramaCore's reader declares the stream when it connects (ensure_queue_exists sends a stream
// create), and RabbitMQ checks configure permission on the stream before it answers "already
// exists". So the account may configure and read oramacore-operations, and write nothing.
func (r *OramaCloudV1Reconciler) reconcileRemoteReaderUser(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	pass, err := randHex(24)
	if err != nil {
		return err
	}
	if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: remoteReaderPasswordSecretName(oc), Namespace: oc.Namespace, Labels: labelsFor(oc, remoteReadComponent)},
		StringData: map[string]string{"password": pass},
	}); err != nil {
		return err
	}
	stream := "^" + brokerOperationsStream + "$"
	topo := &oramav1alpha1.RabbitMQTopology{ObjectMeta: metav1.ObjectMeta{Name: remoteReadTopologyName(oc), Namespace: oc.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, topo, func() error {
		topo.Labels = labelsFor(oc, remoteReadComponent)
		topo.Spec = oramav1alpha1.RabbitMQTopologySpec{
			BrokerService:  oc.Name + "-broker",
			ManagementPort: 15672,
			AdminSecretRef: brokerAdminSecretName(oc),
			Users: []oramav1alpha1.RabbitMQUser{{Name: remoteReaderBrokerUser,
				PasswordSecretRef: oramav1alpha1.RabbitMQSecretKeyRef{Name: remoteReaderPasswordSecretName(oc), Key: "password"}}},
			Permissions: []oramav1alpha1.RabbitMQPermission{{User: remoteReaderBrokerUser, VHost: "/", Configure: stream, Write: "^$", Read: stream}},
		}
		return controllerutil.SetControllerReference(oc, topo, r.Scheme)
	})
	return err
}

// reconcileRemoteReadSecret copies what a read member needs into <cr>-remote-read: the broker CA,
// the remote reader account, the master API key, and, with an in-cluster control DB, CNPG's server
// CA and streaming_replica client certificate (from <cluster>-ca and <cluster>-replication, so a
// CNPG rotation reaches it on the next pass). A source that does not exist yet is left out;
// remoteReadObservation reports the Secret incomplete until it is there.
func (r *OramaCloudV1Reconciler) reconcileRemoteReadSecret(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	data := map[string][]byte{remoteReadBrokerUserKey: []byte(remoteReaderBrokerUser)}
	masterName, masterKey := masterAPIKeyRef(oc)
	type source struct{ secret, key, as string }
	sources := []source{
		{brokerTLSSecretName(oc), "ca.crt", remoteReadBrokerCAKey},
		{remoteReaderPasswordSecretName(oc), "password", remoteReadBrokerPasswordKey},
		{masterName, masterKey, masterAPIKeyKey},
	}
	if controlDBInCluster(oc) {
		cluster := controlDBClusterName(oc)
		sources = append(sources,
			source{cluster + "-ca", "ca.crt", remoteReadControlDBCAKey},
			source{cluster + "-replication", "tls.crt", remoteReadControlDBCertKey},
			source{cluster + "-replication", "tls.key", remoteReadControlDBKeyKey})
	}
	for _, s := range sources {
		sec := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: s.secret}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if v := secretValue(sec, s.key); v != "" {
			data[s.as] = []byte(v)
		}
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: remoteReadSecretName(oc), Namespace: oc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = labelsFor(oc, remoteReadComponent)
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = data
		return controllerutil.SetControllerReference(oc, sec, r.Scheme)
	})
	return err
}

// remoteReadKeys are the keys a read member needs from the credentials Secret; replica adds the
// control database's.
func remoteReadKeys(replica bool) []string {
	keys := []string{remoteReadBrokerCAKey, remoteReadBrokerUserKey, remoteReadBrokerPasswordKey, masterAPIKeyKey}
	if replica {
		keys = append(keys, remoteReadControlDBCAKey, remoteReadControlDBCertKey, remoteReadControlDBKeyKey)
	}
	return keys
}

// missingKeys lists the keys a Secret lacks or holds empty, in the order asked.
func missingKeys(sec *corev1.Secret, keys []string) []string {
	var missing []string
	for _, k := range keys {
		if secretValue(sec, k) == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

// remoteReadObservation is what a primary has published: status.remoteRead once everything is in
// place, else why not ("" once published).
func (r *OramaCloudV1Reconciler) remoteReadObservation(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*oramav1alpha1.RemoteReadStatus, string, error) {
	get := func(name string, obj client.Object) (bool, error) {
		err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, obj)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	brokerSvc := &corev1.Service{}
	if found, err := get(brokerRemoteServiceName(oc), brokerSvc); err != nil || !found {
		return nil, "waiting for Service " + brokerRemoteServiceName(oc), err
	}
	brokerAddress := loadBalancerAddress(brokerSvc)
	if brokerAddress == "" {
		return nil, "waiting for Service " + brokerRemoteServiceName(oc) + " to get a load-balancer address", nil
	}
	tls := &corev1.Secret{}
	if found, err := get(brokerTLSSecretName(oc), tls); err != nil || !found || len(tls.Data["tls.crt"]) == 0 {
		return nil, "waiting for the broker's server certificate", err
	}
	// The broker serves the certificate only once it has restarted with it.
	sts := &appsv1.StatefulSet{}
	if found, err := get(oc.Name+"-broker", sts); err != nil || !found {
		return nil, "waiting for the broker", err
	}
	want := replicasOf(sts.Spec.Replicas)
	if sts.Spec.Template.Annotations[brokerTLSCertAnnotation] != tlsMaterialHash(tls) || sts.Status.ObservedGeneration < sts.Generation ||
		sts.Status.UpdatedReplicas < want || sts.Status.ReadyReplicas < want || sts.Status.CurrentRevision != sts.Status.UpdateRevision {
		return nil, "waiting for the broker to restart with its TLS stream listener", nil
	}
	published := &oramav1alpha1.RemoteReadStatus{
		BrokerStreamEndpoint: net.JoinHostPort(brokerAddress, strconv.Itoa(brokerStreamTLSPort)),
		CredentialsSecret:    remoteReadSecretName(oc),
	}
	if controlDBInCluster(oc) {
		dbSvc := &corev1.Service{}
		if found, err := get(controlDBRemoteServiceName(oc), dbSvc); err != nil || !found || loadBalancerAddress(dbSvc) == "" {
			return nil, "waiting for Service " + controlDBRemoteServiceName(oc) + " to get a load-balancer address", err
		}
		published.ControlDBEndpoint = net.JoinHostPort(loadBalancerAddress(dbSvc), strconv.Itoa(controlDBPort))
	}
	handoff := &corev1.Secret{}
	if found, err := get(remoteReadSecretName(oc), handoff); err != nil || !found {
		return nil, "waiting for Secret " + remoteReadSecretName(oc), err
	}
	if missing := missingKeys(handoff, remoteReadKeys(controlDBInCluster(oc))); len(missing) > 0 {
		return nil, "Secret " + remoteReadSecretName(oc) + " is missing " + strings.Join(missing, ", "), nil
	}
	return published, "", nil
}

// peerProblem says why a read member cannot read from its primary yet, or "" when it can: a peer
// with a usable stream endpoint and a credentials Secret holding every key it needs, plus a
// postgresEndpoint when the control DB replicates.
func (r *OramaCloudV1Reconciler) peerProblem(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	p := remotePeer(oc)
	if p == nil {
		return "spec.peers names no primary with brokerStreamEndpoint and credentialsSecretRef, so the readers have no stream to follow", nil
	}
	if _, _, err := net.SplitHostPort(p.BrokerStreamEndpoint); err != nil {
		return fmt.Sprintf("peer %s: brokerStreamEndpoint %q is not host:port", p.Name, p.BrokerStreamEndpoint), nil
	}
	replica := controlDBIsReplica(oc) && controlDBInCluster(oc)
	if replica {
		if _, _, err := net.SplitHostPort(p.PostgresEndpoint); err != nil {
			return fmt.Sprintf("controlDB.replicate is set, and peer %s has no postgresEndpoint (host:port)", p.Name), nil
		}
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: p.CredentialsSecretRef.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("Secret %s (peer %s's remote-read credentials) does not exist", p.CredentialsSecretRef.Name, p.Name), nil
		}
		return "", err
	}
	if missing := missingKeys(sec, remoteReadKeys(replica)); len(missing) > 0 {
		return fmt.Sprintf("Secret %s (peer %s's remote-read credentials) is missing %s", sec.Name, p.Name, strings.Join(missing, ", ")), nil
	}
	return "", nil
}

// crossClusterReadiness is the component entry and condition of the cross-cluster read path, for
// applyReadiness: "remote-read" and RemoteReadPublished on a publishing primary, "peer" and
// PeerReady on a read member, neither elsewhere. remoteRead is status.remoteRead.
type crossClusterReadiness struct {
	component  *oramav1alpha1.ComponentStatus
	condition  *metav1.Condition
	remoteRead *oramav1alpha1.RemoteReadStatus
}

func (r *OramaCloudV1Reconciler) crossClusterReadiness(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (crossClusterReadiness, error) {
	switch {
	case remoteReadActive(oc):
		published, why, err := r.remoteReadObservation(ctx, oc)
		if err != nil {
			return crossClusterReadiness{}, err
		}
		if why != "" {
			return crossClusterReadiness{
				component: &oramav1alpha1.ComponentStatus{Name: remoteReadComponent, Detail: "waiting"},
				condition: &metav1.Condition{Type: remoteReadConditionType, Status: metav1.ConditionFalse, Reason: "Waiting", Message: why},
			}, nil
		}
		message := "stream at " + published.BrokerStreamEndpoint
		if published.ControlDBEndpoint != "" {
			message += ", control DB at " + published.ControlDBEndpoint
		}
		return crossClusterReadiness{
			component:  &oramav1alpha1.ComponentStatus{Name: remoteReadComponent, Ready: true, Detail: "published"},
			condition:  &metav1.Condition{Type: remoteReadConditionType, Status: metav1.ConditionTrue, Reason: "Published", Message: message + "; credentials in Secret " + published.CredentialsSecret},
			remoteRead: published,
		}, nil
	case oc.Spec.Region.Role == "read":
		problem, err := r.peerProblem(ctx, oc)
		if err != nil {
			return crossClusterReadiness{}, err
		}
		if problem != "" {
			return crossClusterReadiness{
				component: &oramav1alpha1.ComponentStatus{Name: peerComponent, Detail: "waiting"},
				condition: &metav1.Condition{Type: peerConditionType, Status: metav1.ConditionFalse, Reason: "PeerNotReady", Message: problem},
			}, nil
		}
		p := remotePeer(oc)
		return crossClusterReadiness{
			component: &oramav1alpha1.ComponentStatus{Name: peerComponent, Ready: true, Detail: p.Name},
			condition: &metav1.Condition{Type: peerConditionType, Status: metav1.ConditionTrue, Reason: "PeerReady",
				Message: "reading from " + p.Name + " at " + p.BrokerStreamEndpoint},
		}, nil
	}
	return crossClusterReadiness{}, nil
}

// ---- certificates ----

func newECKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// mintBrokerCA creates the CA that signs the broker's server certificate.
func mintBrokerCA(oc *oramav1alpha1.OramaCloudV1, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, keyPEM, err := newECKey()
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: oc.Namespace + "/" + oc.Name + " broker CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(brokerCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// parseCA reads the CA certificate and key, failing when either is missing or they do not match.
func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("no CA")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) || !cert.IsCA {
		return nil, nil, fmt.Errorf("the CA key does not match its certificate")
	}
	return cert, key, nil
}

// issueBrokerServerCert signs a server certificate for the broker's names and the remote address
// (an IP SAN for an IP, a DNS SAN for a host name).
func issueBrokerServerCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey, names []string, address string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, keyPEM, err := newECKey()
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(brokerServerCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     slices.Clone(names),
	}
	if ip := net.ParseIP(address); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = append(template.DNSNames, address)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// serverCertCovers reports whether a server certificate is signed by the CA, names every in-cluster
// name and the address, and has more than the renewal window left.
func serverCertCovers(certPEM []byte, ca *x509.Certificate, names []string, address string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.CheckSignatureFrom(ca) != nil || now.Add(brokerServerCertRenewal).After(cert.NotAfter) {
		return false
	}
	for _, name := range append(slices.Clone(names), address) {
		if cert.VerifyHostname(name) != nil {
			return false
		}
	}
	return true
}
