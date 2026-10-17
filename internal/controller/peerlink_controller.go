/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	peerLinkFinalizer = "orama.cloud/peer-link"
	peerLinkLabel     = "orama.cloud/peer-link"
	peerLinkPoll      = 30 * time.Second
)

// peerEndIdentity is an end's certificate identity, and what the other end requires (--peer-name).
func peerEndIdentity(end, peerID string) string { return end + "." + peerID + ".peer.orama.internal" }

func peerLinkCASecret(link *oramav1alpha1.PeerLink) string { return "peerlink-" + link.Name }

// PeerClusterClients gives a client for the cluster an end runs on. An empty clusterRef is this
// cluster. waiting explains why the cluster cannot be reached yet.
type PeerClusterClients interface {
	ClientFor(ctx context.Context, clusterRef, namespace string) (c client.Client, waiting string, err error)
}

// managedPeerClusterClients reaches managed clusters with their KubernetesCluster kubeconfig.
type managedPeerClusterClients struct{ local client.Client }

func (m managedPeerClusterClients) ClientFor(ctx context.Context, clusterRef, namespace string) (client.Client, string, error) {
	if clusterRef == "" {
		return m.local, "", nil
	}
	kubeconfig, waiting, err := managedClusterKubeconfig(ctx, m.local, clusterRef)
	if err != nil || waiting != "" {
		return nil, waiting, err
	}
	_, c, err := leafClients(kubeconfig, namespace)
	return c, "", err
}

// PeerLinkReconciler gives the two ends of a peer their credentials, through the managing cluster:
// it mints the pair's trust root once, issues one certificate per end (client and server, the end's
// exact identity), and writes each end's credentials Secret on that end's cluster with the other
// end's identity and, once known, its address. Deleting the PeerLink deletes both Secrets.
type PeerLinkReconciler struct {
	client.Client
	// APIReader reads the managing cluster's own Namespaces uncached (operator#106). Nil uses Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// OperatorNamespace holds the pair's CA Secret. Empty means orama-system.
	OperatorNamespace string
	// Clusters is injected for tests; nil reaches managed clusters by their kubeconfig.
	Clusters PeerClusterClients
	Now      func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=peerlinks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=peerlinks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=peerlinks/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=kubernetesclusters,verbs=get;list;watch

func (r *PeerLinkReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PeerLinkReconciler) clusters() PeerClusterClients {
	if r.Clusters != nil {
		return r.Clusters
	}
	return managedPeerClusterClients{local: r.Client}
}

func (r *PeerLinkReconciler) namespace() string {
	if r.OperatorNamespace != "" {
		return r.OperatorNamespace
	}
	return "orama-system"
}

func (r *PeerLinkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var link oramav1alpha1.PeerLink
	if err := r.Get(ctx, req.NamespacedName, &link); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !link.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&link, peerLinkFinalizer) {
			return ctrl.Result{}, nil
		}
		for _, end := range link.Spec.Ends {
			if err := r.removeEnd(ctx, &link, end); err != nil {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(&link, peerLinkFinalizer)
		return ctrl.Result{}, r.Update(ctx, &link)
	}
	if !controllerutil.ContainsFinalizer(&link, peerLinkFinalizer) {
		controllerutil.AddFinalizer(&link, peerLinkFinalizer)
		if err := r.Update(ctx, &link); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if parsed, err := uuid.Parse(link.Spec.PeerID); err != nil || parsed.String() != link.Spec.PeerID || len(link.Spec.Ends) != 2 ||
		link.Spec.Ends[0].Name == link.Spec.Ends[1].Name {
		return ctrl.Result{}, r.setStatus(ctx, &link, oramav1alpha1.PeerLinkStatus{Phase: oramav1alpha1.PeerPhaseFailed},
			metav1.ConditionFalse, "Invalid", "a PeerLink has a canonical peerId and two differently named ends")
	}
	material, err := r.material(ctx, &link)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Read each end's address first, so each Secret can carry the other's.
	addresses := make([]string, 2)
	messages := make([]string, 2)
	for i, end := range link.Spec.Ends {
		addresses[i], messages[i], err = r.endAddress(ctx, &link, end)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	status := oramav1alpha1.PeerLinkStatus{Phase: oramav1alpha1.PeerPhaseReady}
	for i, end := range link.Spec.Ends {
		other := link.Spec.Ends[1-i]
		written, message, err := r.writeEnd(ctx, &link, end, material[i], peerEndIdentity(other.Name, link.Spec.PeerID), addresses[1-i])
		if err != nil {
			return ctrl.Result{}, err
		}
		if message == "" {
			message = messages[i]
		}
		if !written || addresses[i] == "" {
			status.Phase = oramav1alpha1.PeerPhasePending
		}
		status.Ends = append(status.Ends, oramav1alpha1.PeerLinkEndStatus{
			Name: end.Name, Identity: peerEndIdentity(end.Name, link.Spec.PeerID), Written: written, Address: addresses[i], Message: message,
		})
	}
	conditionStatus, reason, message := metav1.ConditionTrue, "CredentialsDelivered", "both ends hold current credentials and know each other's address"
	if status.Phase != oramav1alpha1.PeerPhaseReady {
		conditionStatus, reason, message = metav1.ConditionFalse, "Waiting", "an end has no credentials yet or no address yet"
	}
	return ctrl.Result{RequeueAfter: peerLinkPoll}, r.setStatus(ctx, &link, status, conditionStatus, reason, message)
}

// peerEndMaterial is one end's certificate and key, kept on the managing cluster.
type peerEndMaterial struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

// material mints the pair's CA once and keeps each end's leaf current, in one Secret in the
// operator's namespace on the managing cluster.
func (r *PeerLinkReconciler) material(ctx context.Context, link *oramav1alpha1.PeerLink) ([]peerEndMaterial, error) {
	now := r.now()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: peerLinkCASecret(link), Namespace: r.namespace()}}
	var material []peerEndMaterial
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, link) {
			return fmt.Errorf("Secret %s/%s is not this PeerLink's", secret.Namespace, secret.Name)
		}
		secret.Labels = map[string]string{peerLinkLabel: link.Name, workspaceManagedByLabel: zettaTunnelManagedBy}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		ca := tunnelCA{CertPEM: secret.Data["ca.crt"], KeyPEM: secret.Data["ca.key"]}
		if _, _, err := parseTunnelCA(ca); err != nil {
			fresh, err := newTunnelCA("peer "+link.Spec.PeerID+" CA", now)
			if err != nil {
				return err
			}
			ca = fresh
			secret.Data = map[string][]byte{"ca.crt": ca.CertPEM, "ca.key": ca.KeyPEM}
		}
		material = nil
		for _, end := range link.Spec.Ends {
			request := tunnelLeafRequest{
				CommonName: peerEndIdentity(end.Name, link.Spec.PeerID),
				DNSNames:   []string{peerEndIdentity(end.Name, link.Spec.PeerID)},
				Server:     true,
				Client:     true,
			}
			certKey, keyKey := end.Name+".crt", end.Name+".key"
			if !tunnelLeafCurrent(secret.Data[certKey], secret.Data[keyKey], ca.CertPEM, request, now) {
				cert, key, err := issueTunnelLeaf(ca, request, now)
				if err != nil {
					return err
				}
				secret.Data[certKey], secret.Data[keyKey] = cert, key
			}
			material = append(material, peerEndMaterial{CertPEM: secret.Data[certKey], KeyPEM: secret.Data[keyKey], CAPEM: ca.CertPEM})
		}
		return controllerutil.SetControllerReference(link, secret, r.Scheme)
	})
	return material, err
}

func (r *PeerLinkReconciler) endAddress(ctx context.Context, link *oramav1alpha1.PeerLink, end oramav1alpha1.PeerLinkEnd) (string, string, error) {
	c, waiting, err := r.clusters().ClientFor(ctx, end.ClusterRef, end.Namespace)
	if err != nil || waiting != "" {
		return "", waiting, err
	}
	var peer oramav1alpha1.PeerConnection
	if err := c.Get(ctx, client.ObjectKey{Namespace: end.Namespace, Name: end.PeerConnection}, &peer); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return "", fmt.Sprintf("PeerConnection %s/%s does not exist yet", end.Namespace, end.PeerConnection), nil
		}
		return "", "", err
	}
	if peer.Spec.PeerID != link.Spec.PeerID {
		return "", fmt.Sprintf("PeerConnection %s/%s carries peer %s, not %s", end.Namespace, end.PeerConnection, peer.Spec.PeerID, link.Spec.PeerID), nil
	}
	return peer.Status.Address, "", nil
}

func peerLinkEndSecretName(end oramav1alpha1.PeerLinkEnd) string {
	return end.PeerConnection + "-credentials"
}

func (r *PeerLinkReconciler) writeEnd(
	ctx context.Context,
	link *oramav1alpha1.PeerLink,
	end oramav1alpha1.PeerLinkEnd,
	material peerEndMaterial,
	peerName string,
	remote string,
) (bool, string, error) {
	c, waiting, err := r.clusters().ClientFor(ctx, end.ClusterRef, end.Namespace)
	if err != nil || waiting != "" {
		return false, waiting, err
	}
	var namespace corev1.Namespace
	reader := client.Reader(c)
	if end.ClusterRef == "" && r.APIReader != nil {
		// The managing cluster's own client is cached; read its Namespaces uncached (operator#106).
		reader = r.APIReader
	}
	if err := reader.Get(ctx, client.ObjectKey{Name: end.Namespace}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("namespace %s does not exist on the end's cluster yet", end.Namespace), nil
		}
		return false, "", err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: peerLinkEndSecretName(end), Namespace: end.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && secret.Labels[peerLinkLabel] != link.Name {
			return errPeerSecretForeign
		}
		secret.Labels = map[string]string{peerLinkLabel: link.Name, peerIDLabel: link.Spec.PeerID, workspaceManagedByLabel: zettaTunnelManagedBy}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			corev1.TLSCertKey:       material.CertPEM,
			corev1.TLSPrivateKeyKey: material.KeyPEM,
			"ca.crt":                material.CAPEM,
			"peer-name":             []byte(peerName),
		}
		if remote != "" {
			secret.Data["remote"] = []byte(remote)
		}
		return nil
	})
	if errors.Is(err, errPeerSecretForeign) {
		return false, fmt.Sprintf("Secret %s/%s exists and is not this PeerLink's", end.Namespace, secret.Name), nil
	}
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

var errPeerSecretForeign = errors.New("the Secret is not this PeerLink's")

func (r *PeerLinkReconciler) removeEnd(ctx context.Context, link *oramav1alpha1.PeerLink, end oramav1alpha1.PeerLinkEnd) error {
	c, waiting, err := r.clusters().ClientFor(ctx, end.ClusterRef, end.Namespace)
	if err != nil {
		return err
	}
	if waiting != "" {
		// A cluster that is gone holds nothing to remove.
		return nil
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: end.Namespace, Name: peerLinkEndSecretName(end)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if secret.Labels[peerLinkLabel] != link.Name {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, &secret))
}

func (r *PeerLinkReconciler) setStatus(
	ctx context.Context,
	link *oramav1alpha1.PeerLink,
	observed oramav1alpha1.PeerLinkStatus,
	conditionStatus metav1.ConditionStatus,
	reason string,
	message string,
) error {
	desired := observed
	desired.Conditions = append([]metav1.Condition(nil), link.Status.Conditions...)
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{
		Type: tunnelReadyCondition, Status: conditionStatus, Reason: reason, Message: message, ObservedGeneration: link.Generation,
	})
	if apiequality.Semantic.DeepEqual(link.Status, desired) {
		return nil
	}
	link.Status = desired
	return r.Status().Update(ctx, link)
}

func (r *PeerLinkReconciler) linksForLocalPeer(ctx context.Context, object client.Object) []reconcile.Request {
	var links oramav1alpha1.PeerLinkList
	if err := r.List(ctx, &links); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range links.Items {
		for _, end := range links.Items[i].Spec.Ends {
			if end.ClusterRef == "" && end.Namespace == object.GetNamespace() && end.PeerConnection == object.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&links.Items[i])})
			}
		}
	}
	return requests
}

func (r *PeerLinkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.PeerLink{}).
		Watches(&oramav1alpha1.PeerConnection{}, handler.EnqueueRequestsFromMapFunc(r.linksForLocalPeer)).
		Complete(r)
}
