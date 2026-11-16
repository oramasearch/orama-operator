// SPDX-License-Identifier: MIT

package controller

// Zetta identity and enrollment (operator#1174, zetta PR #1512 and PR #1514).
//
// Two things the engine now needs from whoever runs it:
//
//  1. Rotating ZETTA_ADMIN_TOKEN (zetta#1342). The identity root is derived from the admin token and
//     moves by succession: rotate_identity_root {new_admin_token} must run on a node that still holds
//     the current root BEFORE any pod starts with the new token, or every pod comes back verify-only.
//     The operator does it when spec.identity.adminTokenGeneration is raised:
//       a. it mints the new token into Secret <name>-zetta-admin under key `next` (the pods read key
//          `token`, so nothing restarts with it yet);
//       b. once every node is ready on the current token, it calls rotate_identity_root on node 0 with
//          the current token as bearer. An answer that the new token "derives the current root" means a
//          previous call already did it;
//       c. it moves `next` into `token` and records the generation, then stamps the generation on the
//          pod template, so the StatefulSet rolls the new token. A pod restarted with it follows the
//          succession and issues identity again.
//     Tokens are never revoked (revoke_tokens stays false): the old token keeps working for the other
//     holders of the Secret (lake's data-plane variables, workspace credentials) until they restart.
//     A rotation waits for an image whose usage lists ZETTA_IDENTITY_MIN_EPOCH (the same zetta release
//     as rotate_identity_root) and runs only on a cluster of one slice: the other slices of a cluster
//     spanning Kubernetes clusters hold tokens this operator does not.
//
//  2. The enrollment CA (zetta#1513). After zetta PR #1514 a node that is not enrolled joins only
//     through a member the CA signed, so joiners need ZETTA_CLUSTER_CA (the CA's public key; public,
//     like ZETTA_IDENTITY_ROOT, so a literal variable), and in an operator-run cluster, where every
//     pod gets ZETTA_CLUSTER_TOKEN from outside and no pod creates the CA, some pods must hold its seed.
//     Secret <name>-zetta-cluster-ca records it, once:
//       - a NEW cluster (no StatefulSet yet) whose image reads ZETTA_CLUSTER_CA_SEED_FILE gets a seed
//         (32 random bytes as 64 lowercase hex) minted once, key `seed`, and its public key, key
//         `public`. Every node of this slice mounts the seed read-only as a file and holds the CA;
//       - a cluster that already ran zetta PR #1512 holds a CA its nodes generated (cluster-ca.json in
//         each data dir). A seed would conflict with it and stop the node, so none is minted. Once a
//         node runs an image that reads ZETTA_CLUSTER_CA, the operator reads the CA's public key from
//         that node's printed join command (`--cluster-ca <key>`) and records it under `public`;
//       - an ephemeral (burst) slice never holds the CA: it gets ZETTA_CLUSTER_CA of a non-ephemeral
//         slice of the same cluster in its namespace, and enrolls through those nodes.
//     ZETTA_CLUSTER_CA is set only for an image whose usage lists it, and only once the key is known.
//     The seed is never in an environment variable, a log or the status.
//
// ZETTA_IDENTITY_MIN_EPOCH (zetta#1360) is optional and deliberately not set: a recorded floor
// would leave a cluster rebuilt from nothing, or a single-node workspace Zetta that lost its volume,
// honoring no account or token until the floor is cleared by hand. See
// docs/zetta-cluster-mtls.md#identity-epoch-floor-not-set.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// The engine's variables, and what its usage lists when it has them.
	zettaIdentityMinEpochEnv  = "ZETTA_IDENTITY_MIN_EPOCH"
	zettaClusterCAEnv         = "ZETTA_CLUSTER_CA"
	zettaClusterCASeedFileEnv = "ZETTA_CLUSTER_CA_SEED_FILE"

	// The admin token Secret's keys: the token the pods read, its generation, and a rotation's new
	// token until rotate_identity_root has run.
	zettaAdminTokenKey          = "token"
	zettaAdminGenerationKey     = "generation"
	zettaAdminNextKey           = "next"
	zettaAdminNextGenerationKey = "next-generation"
	// zettaAdminGenerationAnnotation on the pod template rolls the new token after a rotation. It is
	// absent at generation 1, so a cluster that never rotates keeps its template.
	zettaAdminGenerationAnnotation = "orama.cloud/zetta-admin-generation"
	// zettaIdentityConditionType exists only on a cluster with spec.identity.adminTokenGeneration.
	zettaIdentityConditionType = "IdentityRootRotated"
	// zettaIdentityRetry is how soon a rotation that could not run yet is looked at again.
	zettaIdentityRetry = 30 * time.Second

	// The enrollment CA Secret: <name>-zetta-cluster-ca, keys seed (only where the operator minted the
	// CA) and public.
	zettaClusterCASuffix     = "-zetta-cluster-ca"
	zettaClusterCASeedKey    = "seed"
	zettaClusterCAPublicKey  = "public"
	zettaClusterCAVolume     = "cluster-ca"
	zettaClusterCAMountPath  = "/run/secrets/zetta-cluster-ca"
	zettaClusterCASeedFile   = zettaClusterCAMountPath + "/" + zettaClusterCASeedKey
	zettaClusterCASourceOps  = "Operator"
	zettaClusterCASourceMem  = "Members"
	zettaClusterCASourceSib  = "Sibling"
	zettaNodeContainer       = "zetta"
	zettaNodeLogLimit        = int64(1 << 20)
	zettaAlreadyRotatedNotes = "derives the current root"
)

// zettaJoinCommandCA finds the CA key in a node's printed join command.
var zettaJoinCommandCA = regexp.MustCompile(`--cluster-ca ([0-9a-f]{64})\b`)

// zettaClusterCASecretName names a slice's enrollment CA Secret.
func zettaClusterCASecretName(name string) string { return name + zettaClusterCASuffix }

// zettaIdentityRender is what the StatefulSet carries for the identity root and the enrollment CA.
type zettaIdentityRender struct {
	// adminGeneration above 1 is stamped on the pod template.
	adminGeneration int64
	// clusterCA is ZETTA_CLUSTER_CA; empty sets nothing.
	clusterCA string
	// caSeed mounts the seed of this slice's Secret as ZETTA_CLUSTER_CA_SEED_FILE.
	caSeed bool
	// tokenFileOnly is true when the image reads the join token from its file (operator#721):
	// ZETTA_CLUSTER_TOKEN is then not set, so the token is not in the container's environment.
	tokenFileOnly bool
}

// zettaIdentityState is what one reconcile knows about the cluster's identity.
type zettaIdentityState struct {
	render zettaIdentityRender
	// hold keeps a new cluster's StatefulSet from being created until its image's answer says
	// whether it gets a CA seed: created without one, its nodes could never hold the CA.
	hold      bool
	status    oramav1alpha1.ZettaIdentityStatus
	condition *metav1.Condition
	requeue   time.Duration
}

// zettaStandalone reports whether the cluster is this slice alone: no sibling slice named by
// spec.clusterName elsewhere and no external peer.
func zettaStandalone(z *oramav1alpha1.OramaZettaCluster) bool {
	return (z.Spec.ClusterName == "" || z.Spec.ClusterName == z.Name) && len(z.Spec.ExternalPeers) == 0
}

// zettaFeaturesFor returns the answer when it is for spec.image, else nil.
func zettaFeaturesFor(z *oramav1alpha1.OramaZettaCluster, features *oramav1alpha1.ZettaImageFeatures) *oramav1alpha1.ZettaImageFeatures {
	if features == nil || features.Image != zettaImage(z) {
		return nil
	}
	return features
}

// zettaClusterCAPublicFromSeed is the Ed25519 public key of a 64-hex seed, as zetta derives it.
func zettaClusterCAPublicFromSeed(seedHex string) (string, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize || strings.ToLower(seedHex) != seedHex {
		return "", errors.New("the enrollment CA seed is not 64 lowercase hex characters")
	}
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return hex.EncodeToString(public), nil
}

// reconcileZettaIdentity decides the identity render for this reconcile: the enrollment CA, then a
// pending admin token rotation.
func (r *OramaZettaClusterReconciler) reconcileZettaIdentity(
	ctx context.Context,
	z *oramav1alpha1.OramaZettaCluster,
	features *oramav1alpha1.ZettaImageFeatures,
	stsName, headlessName string,
) (zettaIdentityState, error) {
	state := zettaIdentityState{}
	sts := &appsv1.StatefulSet{}
	stsExists := true
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: stsName}, sts); err != nil {
		if !apierrors.IsNotFound(err) {
			return state, err
		}
		stsExists = false
	}
	if err := r.reconcileZettaClusterCA(ctx, z, zettaFeaturesFor(z, features), stsName, stsExists, &state); err != nil {
		return state, err
	}
	if err := r.reconcileZettaAdminRotation(ctx, z, zettaFeaturesFor(z, features), sts, stsExists, stsName, headlessName, &state); err != nil {
		return state, err
	}
	return state, nil
}

// reconcileZettaClusterCA keeps Secret <name>-zetta-cluster-ca and decides ZETTA_CLUSTER_CA and the
// seed file.
func (r *OramaZettaClusterReconciler) reconcileZettaClusterCA(
	ctx context.Context,
	z *oramav1alpha1.OramaZettaCluster,
	features *oramav1alpha1.ZettaImageFeatures,
	stsName string,
	stsExists bool,
	state *zettaIdentityState,
) error {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: z.Namespace, Name: zettaClusterCASecretName(z.Name)}
	found := true
	if err := r.Get(ctx, key, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		found = false
	}
	if found && !metav1.IsControlledBy(secret, z) {
		return fmt.Errorf("Secret %s/%s is not controlled by OramaZettaCluster %s", key.Namespace, key.Name, z.Name)
	}
	holder := !z.Spec.Ephemeral && zettaStandalone(z)

	switch {
	case !found && !stsExists && holder:
		// A new cluster: it gets a seed if its image reads one. Until the image has answered, it waits.
		if features == nil {
			if r.zettaPodLogsConfigured() {
				state.hold, state.requeue = true, zettaFeaturesPoll
			}
			break
		}
		if !features.ClusterCASeedFile {
			break
		}
		minted, err := r.mintZettaClusterCA(ctx, z)
		if err != nil {
			return err
		}
		secret, found = minted, true
	case (!found || len(secret.Data[zettaClusterCAPublicKey]) == 0) && stsExists && features != nil && features.ClusterCA && !z.Spec.Ephemeral:
		// A cluster whose nodes generated their CA (zetta PR #1512): learn its key from a node.
		learned, err := r.learnZettaClusterCA(ctx, z, stsName, secret, found)
		if err != nil {
			return err
		}
		if learned != nil {
			secret, found = learned, true
		} else {
			state.requeue = zettaIdentityRetry
		}
	}

	if features == nil || !features.ClusterCA {
		if found {
			state.status.ClusterCA = string(secret.Data[zettaClusterCAPublicKey])
		}
		return nil
	}
	if found && len(secret.Data[zettaClusterCAPublicKey]) > 0 {
		state.render.clusterCA = string(secret.Data[zettaClusterCAPublicKey])
		state.status.ClusterCA = state.render.clusterCA
		state.status.ClusterCASource = zettaClusterCASourceMem
		if len(secret.Data[zettaClusterCASeedKey]) > 0 {
			state.status.ClusterCASource = zettaClusterCASourceOps
			state.render.caSeed = holder && features.ClusterCASeedFile
		}
		return nil
	}
	if z.Spec.Ephemeral || !zettaStandalone(z) {
		sibling, err := r.zettaSiblingClusterCA(ctx, z)
		if err != nil {
			return err
		}
		if sibling != "" {
			state.render.clusterCA = sibling
			state.status.ClusterCA, state.status.ClusterCASource = sibling, zettaClusterCASourceSib
		}
	}
	return nil
}

// mintZettaClusterCA creates the CA Secret with a new seed and its public key. It never overwrites
// one: a Secret that appeared meanwhile is returned as it is.
func (r *OramaZettaClusterReconciler) mintZettaClusterCA(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (*corev1.Secret, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	seedHex := hex.EncodeToString(seed)
	public, err := zettaClusterCAPublicFromSeed(seedHex)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zettaClusterCASecretName(z.Name), Namespace: z.Namespace, Labels: zettaLabels(z)},
		Data: map[string][]byte{
			zettaClusterCASeedKey:   []byte(seedHex),
			zettaClusterCAPublicKey: []byte(public),
		},
	}
	if err := controllerutil.SetControllerReference(z, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(secret), existing); err != nil {
				return nil, err
			}
			return existing, nil
		}
		return nil, fmt.Errorf("create the enrollment CA Secret: %w", err)
	}
	logf.FromContext(ctx).Info("minted the enrollment CA seed — this slice's nodes hold the CA (zetta#1513)",
		"secret", secret.Name, "clusterCA", public)
	return secret, nil
}

// learnZettaClusterCA reads the CA key from node 0's printed join command, when node 0 runs
// spec.image, and records it. nil means not known yet.
func (r *OramaZettaClusterReconciler) learnZettaClusterCA(
	ctx context.Context,
	z *oramav1alpha1.OramaZettaCluster,
	stsName string,
	existing *corev1.Secret,
	found bool,
) (*corev1.Secret, error) {
	if !r.zettaPodLogsConfigured() {
		return nil, nil
	}
	pod := &corev1.Pod{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: stsName + "-0"}, pod); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	running := false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == zettaNodeContainer && status.State.Running != nil {
			running = true
		}
	}
	image := ""
	for _, container := range pod.Spec.Containers {
		if container.Name == zettaNodeContainer {
			image = container.Image
		}
	}
	if !running || image != zettaImage(z) {
		return nil, nil
	}
	log, err := r.zettaNodeLog(ctx, z.Namespace, pod.Name)
	if err != nil {
		return nil, nil
	}
	match := zettaJoinCommandCA.FindStringSubmatch(log)
	if match == nil {
		return nil, nil
	}
	public := match[1]
	if found {
		patched := existing.DeepCopy()
		if patched.Data == nil {
			patched.Data = map[string][]byte{}
		}
		patched.Data[zettaClusterCAPublicKey] = []byte(public)
		if err := r.Update(ctx, patched); err != nil {
			return nil, err
		}
		return patched, nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: zettaClusterCASecretName(z.Name), Namespace: z.Namespace, Labels: zettaLabels(z)},
		Data:       map[string][]byte{zettaClusterCAPublicKey: []byte(public)},
	}
	if err := controllerutil.SetControllerReference(z, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, client.IgnoreAlreadyExists(err)
	}
	logf.FromContext(ctx).Info("recorded the enrollment CA the nodes generated (zetta#1512) for new nodes to join through",
		"secret", secret.Name, "clusterCA", public)
	return secret, nil
}

// zettaNodeLog reads the start of a node's log, where its join command is printed.
func (r *OramaZettaClusterReconciler) zettaNodeLog(ctx context.Context, namespace, podName string) (string, error) {
	if r.PodLogs != nil {
		return r.PodLogs(ctx, namespace, podName)
	}
	limit := zettaNodeLogLimit
	raw, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: zettaNodeContainer, LimitBytes: &limit,
	}).DoRaw(ctx)
	return string(raw), err
}

// zettaSiblingClusterCA is the CA key of a non-ephemeral slice of the same cluster in this namespace,
// the first by name that has one.
func (r *OramaZettaClusterReconciler) zettaSiblingClusterCA(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (string, error) {
	global := z.Spec.ClusterName
	if global == "" {
		global = z.Name
	}
	var clusters oramav1alpha1.OramaZettaClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(z.Namespace)); err != nil {
		return "", err
	}
	slices.SortFunc(clusters.Items, func(a, b oramav1alpha1.OramaZettaCluster) int { return strings.Compare(a.Name, b.Name) })
	for i := range clusters.Items {
		sibling := &clusters.Items[i]
		name := sibling.Spec.ClusterName
		if name == "" {
			name = sibling.Name
		}
		if sibling.Name == z.Name || sibling.Spec.Ephemeral || name != global {
			continue
		}
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: zettaClusterCASecretName(sibling.Name)}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		if public := string(secret.Data[zettaClusterCAPublicKey]); public != "" && metav1.IsControlledBy(&secret, sibling) {
			return public, nil
		}
	}
	return "", nil
}

// zettaAdminGeneration is a generation stored in the admin Secret; absent or unreadable is 1.
func zettaAdminGeneration(raw []byte) int64 {
	generation, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || generation < 1 {
		return 1
	}
	return generation
}

// zettaStatefulSetSettled reports whether every node runs the current template and is ready, so
// node 0 holds the current root.
func zettaStatefulSetSettled(z *oramav1alpha1.OramaZettaCluster, sts *appsv1.StatefulSet) bool {
	replicas := z.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	return sts.Status.ObservedGeneration >= sts.Generation && sts.Status.ReadyReplicas == replicas &&
		sts.Status.UpdatedReplicas == replicas && sts.Status.CurrentRevision == sts.Status.UpdateRevision
}

// reconcileZettaAdminRotation carries out a raised spec.identity.adminTokenGeneration.
func (r *OramaZettaClusterReconciler) reconcileZettaAdminRotation(
	ctx context.Context,
	z *oramav1alpha1.OramaZettaCluster,
	features *oramav1alpha1.ZettaImageFeatures,
	sts *appsv1.StatefulSet,
	stsExists bool,
	stsName, headlessName string,
	state *zettaIdentityState,
) error {
	admin := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: z.Name + "-zetta-admin"}, admin); err != nil {
		return err
	}
	current := zettaAdminGeneration(admin.Data[zettaAdminGenerationKey])
	state.render.adminGeneration = current
	state.status.AdminTokenGeneration = current
	target := z.Spec.Identity.AdminTokenGeneration
	if target == 0 {
		return nil
	}
	condition := &metav1.Condition{Type: zettaIdentityConditionType, ObservedGeneration: z.Generation}
	state.condition = condition
	if target <= current {
		condition.Status, condition.Reason = metav1.ConditionTrue, "Current"
		condition.Message = fmt.Sprintf("the identity root runs on admin token generation %d", current)
		return nil
	}
	condition.Status = metav1.ConditionFalse
	wait := func(reason, message string) error {
		condition.Reason, condition.Message = reason, message
		state.requeue = zettaIdentityRetry
		return nil
	}
	switch {
	case features == nil || !features.IdentityRootRotation:
		return wait("ImageNotSupported", fmt.Sprintf("spec.image's usage does not list %s, so it has no rotate_identity_root; generation %d waits", zettaIdentityMinEpochEnv, target))
	case !zettaStandalone(z):
		return wait("MultiSliceNotSupported", "the cluster spans slices (spec.clusterName or spec.externalPeers) whose admin tokens this operator does not hold, so its identity root is not rotated here")
	case !stsExists || !zettaStatefulSetSettled(z, sts):
		return wait("WaitingForNodes", "every node must be ready on the current admin token before the identity root moves")
	}

	// a. The new token, minted once per target generation, under a key no pod reads.
	if string(admin.Data[zettaAdminNextGenerationKey]) != strconv.FormatInt(target, 10) || len(admin.Data[zettaAdminNextKey]) == 0 {
		next, err := randHex(32)
		if err != nil {
			return err
		}
		patched := admin.DeepCopy()
		patched.Data[zettaAdminNextKey] = []byte(next)
		patched.Data[zettaAdminNextGenerationKey] = []byte(strconv.FormatInt(target, 10))
		if err := r.Update(ctx, patched); err != nil {
			return err
		}
		admin = patched
	}

	// b. The succession, on node 0, with the current token.
	base := r.zettaNodeBaseURL(z, stsName, headlessName)
	if err := zettaRotateIdentityRoot(ctx, base, string(admin.Data[zettaAdminTokenKey]), string(admin.Data[zettaAdminNextKey])); err != nil {
		logf.FromContext(ctx).Info("rotate_identity_root did not run; retrying", "reason", err.Error())
		return wait("RotationFailed", err.Error())
	}

	// c. The new token becomes the one the pods read; the generation rolls them.
	patched := admin.DeepCopy()
	patched.Data[zettaAdminTokenKey] = patched.Data[zettaAdminNextKey]
	patched.Data[zettaAdminGenerationKey] = []byte(strconv.FormatInt(target, 10))
	delete(patched.Data, zettaAdminNextKey)
	delete(patched.Data, zettaAdminNextGenerationKey)
	if err := r.Update(ctx, patched); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("rotated the identity root; the nodes now roll to the new admin token (zetta#1342)",
		"generation", target)
	state.render.adminGeneration = target
	state.status.AdminTokenGeneration = target
	condition.Status, condition.Reason = metav1.ConditionTrue, "Rotated"
	condition.Message = fmt.Sprintf("the identity root moved to admin token generation %d; the nodes roll to it", target)
	return nil
}

// zettaNodeBaseURL is node 0's own API address: the rotation must run on one node that holds the
// current root, and the same node on a retry.
func (r *OramaZettaClusterReconciler) zettaNodeBaseURL(z *oramav1alpha1.OramaZettaCluster, stsName, headlessName string) string {
	if r.ZettaNodeURL != nil {
		return r.ZettaNodeURL(z)
	}
	return fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d", stsName, headlessName, z.Namespace, zettaAPIPort)
}

// zettaRotateIdentityRoot calls POST /v1/rotate_identity_root. A refusal saying the new token derives
// the current root is a rotation already done. The tokens never appear in an error.
func zettaRotateIdentityRoot(ctx context.Context, base, token, next string) error {
	body, err := json.Marshal(map[string]any{"new_admin_token": next, "intent": "operator: rotate the identity root before rolling a new admin token"})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/rotate_identity_root", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("rotate_identity_root: %s", strings.ReplaceAll(err.Error(), token, "****"))
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var answer struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("rotate_identity_root answered %d without a JSON envelope", response.StatusCode)
	}
	if answer.OK {
		return nil
	}
	if strings.Contains(answer.Error.Message, zettaAlreadyRotatedNotes) {
		return nil
	}
	message := answer.Error.Message
	for _, secret := range []string{token, next} {
		message = strings.ReplaceAll(message, secret, "****")
	}
	if len(message) > 200 {
		message = message[:200]
	}
	return fmt.Errorf("rotate_identity_root answered %d %s: %s", response.StatusCode, answer.Error.Code, message)
}

// applyZettaIdentityRender puts the identity render on the pod template.
func applyZettaIdentityRender(z *oramav1alpha1.OramaZettaCluster, template *corev1.PodTemplateSpec, render zettaIdentityRender) {
	if render.adminGeneration > 1 {
		if template.Annotations == nil {
			template.Annotations = map[string]string{}
		}
		template.Annotations[zettaAdminGenerationAnnotation] = strconv.FormatInt(render.adminGeneration, 10)
	}
	if len(template.Spec.Containers) == 0 {
		return
	}
	container := &template.Spec.Containers[0]
	if render.clusterCA != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: zettaClusterCAEnv, Value: render.clusterCA})
	}
	if !render.caSeed {
		return
	}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: zettaClusterCAVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: zettaClusterCASecretName(z.Name),
			Items:      []corev1.KeyToPath{{Key: zettaClusterCASeedKey, Path: zettaClusterCASeedKey}},
			// 0440 as the join token: Secret files are root's, so the zetta user reads it through its
			// fsGroup; no one else can.
			DefaultMode: int32Ptr(zettaClusterTokenFileMode),
		}},
	})
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: zettaClusterCAVolume, MountPath: zettaClusterCAMountPath, ReadOnly: true})
	container.Env = append(container.Env, corev1.EnvVar{Name: zettaClusterCASeedFileEnv, Value: zettaClusterCASeedFile})
}
