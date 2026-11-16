// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The usages of zetta PR #1512 (ZETTA_IDENTITY_MIN_EPOCH) and PR #1514 (the enrollment CA).
const (
	testZettaUsageIdentity   = "  --identity-min-epoch N  ... (or ZETTA_IDENTITY_MIN_EPOCH).\n"
	testZettaUsageEnrollment = testZettaUsageIdentity +
		"  --cluster-ca KEY    ... join command; or ZETTA_CLUSTER_CA). A node that is not\n" +
		"  --cluster-ca-seed-file PATH  ... (or\n                      ZETTA_CLUSTER_CA_SEED_FILE); the node holds the CA (#1513)\n"
	testZettaCAPublic = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func identityFeatures(image string, usage string) *oramav1alpha1.ZettaImageFeatures {
	return parseZettaImageFeatures(image, usage, 0)
}

func identityTestReconciler(t *testing.T, objects ...client.Object) (*OramaZettaClusterReconciler, client.Client) {
	t.Helper()
	scheme := zettaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&appsv1.StatefulSet{}).Build()
	return &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}, cl
}

func adminSecretFor(z *oramav1alpha1.OramaZettaCluster, token string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: z.Name + "-zetta-admin", Namespace: z.Namespace},
		Data: map[string][]byte{"token": []byte(token)}}
}

// operator#1174: each identity setting is what the image's usage says.
func TestZettaImageFeaturesReadTheIdentityVariables(t *testing.T) {
	older := identityFeatures("z:old", testZettaUsageWithFile)
	if older.IdentityRootRotation || older.ClusterCA || older.ClusterCASeedFile {
		t.Fatalf("an older usage: %+v", older)
	}
	first := identityFeatures("z:1512", testZettaUsageIdentity)
	if !first.IdentityRootRotation || first.ClusterCA || first.ClusterCASeedFile {
		t.Fatalf("zetta PR #1512: %+v", first)
	}
	second := identityFeatures("z:1514", testZettaUsageEnrollment)
	if !second.IdentityRootRotation || !second.ClusterCA || !second.ClusterCASeedFile {
		t.Fatalf("zetta PR #1514: %+v", second)
	}
	if crashed := parseZettaImageFeatures("z:1514", testZettaUsageEnrollment, 1); crashed.ClusterCA || crashed.IdentityRootRotation {
		t.Fatal("an unclean answer counts as an image without the features")
	}
}

// The public key is the Ed25519 key of the seed, as zetta derives it.
func TestTheEnrollmentCAKeyIsTheSeedsEd25519Key(t *testing.T) {
	seed := strings.Repeat("ab", 32)
	public, err := zettaClusterCAPublicFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(seed)
	want := hex.EncodeToString(ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey))
	if public != want || len(public) != 64 {
		t.Fatalf("public = %q, want %q", public, want)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("AB", 32), strings.Repeat("ab", 31)} {
		if _, err := zettaClusterCAPublicFromSeed(bad); err == nil {
			t.Errorf("seed %q accepted", bad)
		}
	}
}

// A new cluster whose image reads a seed gets one, minted once; its nodes mount it as a file and are
// told the CA's key. The seed is in no variable.
func TestANewClusterHoldsAnOperatorMintedCA(t *testing.T) {
	ctx := context.Background()
	z := newZettaTestCluster()
	z.Spec.Image = "z:1514"
	r, cl := identityTestReconciler(t, z, adminSecretFor(z, "old-token"))
	features := identityFeatures("z:1514", testZettaUsageEnrollment)
	state, err := r.reconcileZettaIdentity(ctx, z, features, "z0-zetta", "z0-zetta-headless")
	if err != nil || state.hold {
		t.Fatalf("state = %+v, %v", state, err)
	}
	var secret corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: "z0-zetta-cluster-ca"}, &secret); err != nil {
		t.Fatal(err)
	}
	seed, public := string(secret.Data["seed"]), string(secret.Data["public"])
	if derived, err := zettaClusterCAPublicFromSeed(seed); err != nil || derived != public || !metav1.IsControlledBy(&secret, z) {
		t.Fatalf("CA Secret = %+v, %v", secret.ObjectMeta, err)
	}
	if !state.render.caSeed || state.render.clusterCA != public || state.status.ClusterCASource != "Operator" {
		t.Fatalf("render = %+v, status = %+v", state.render, state.status)
	}
	again, _ := r.reconcileZettaIdentity(ctx, z, features, "z0-zetta", "z0-zetta-headless")
	_ = cl.Get(ctx, client.ObjectKeyFromObject(&secret), &secret)
	if string(secret.Data["seed"]) != seed || again.render.clusterCA != public {
		t.Fatal("the seed is minted once")
	}

	sts := renderZettaWithIdentity(t, z, state.render)
	container := sts.Spec.Template.Spec.Containers[0]
	if got := findEnv(container.Env, "ZETTA_CLUSTER_CA"); got == nil || got.Value != public {
		t.Fatalf("ZETTA_CLUSTER_CA = %+v", got)
	}
	if got := findEnv(container.Env, "ZETTA_CLUSTER_CA_SEED_FILE"); got == nil || got.Value != "/run/secrets/zetta-cluster-ca/seed" {
		t.Fatalf("ZETTA_CLUSTER_CA_SEED_FILE = %+v", got)
	}
	for _, variable := range container.Env {
		if variable.Value == seed {
			t.Fatalf("the seed is in variable %s", variable.Name)
		}
	}
	mounted := false
	for _, volume := range sts.Spec.Template.Spec.Volumes {
		if volume.Name == "cluster-ca" {
			mounted = volume.Secret != nil && volume.Secret.SecretName == "z0-zetta-cluster-ca" &&
				len(volume.Secret.Items) == 1 && volume.Secret.Items[0].Key == "seed" && *volume.Secret.DefaultMode == 0o440
		}
	}
	if !mounted {
		t.Fatal("the seed is not mounted as an owner-and-fsGroup file")
	}
}

func renderZettaWithIdentity(t *testing.T, z *oramav1alpha1.OramaZettaCluster, render zettaIdentityRender) *appsv1.StatefulSet {
	t.Helper()
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: z.Namespace}}
	if err := r.mutateZettaStatefulSetWith(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil, zettaEgressRender{}, render); err != nil {
		t.Fatal(err)
	}
	return sts
}

// A new cluster waits for its image's answer before its StatefulSet exists, so it is never created
// without the seed it needs; an older image gets nothing.
func TestANewClusterWaitsForItsImageBeforeItsCAIsDecided(t *testing.T) {
	ctx := context.Background()
	z := newZettaTestCluster()
	z.Spec.Image = "z:1514"
	r, cl := identityTestReconciler(t, z, adminSecretFor(z, "old-token"))
	r.PodLogs = func(context.Context, string, string) (string, error) { return "", nil }
	state, err := r.reconcileZettaIdentity(ctx, z, nil, "z0-zetta", "z0-zetta-headless")
	if err != nil || !state.hold {
		t.Fatalf("state = %+v, %v; want held", state, err)
	}
	state, err = r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1514", testZettaUsageWithFile), "z0-zetta", "z0-zetta-headless")
	if err != nil || state.hold || state.render.clusterCA != "" || state.render.caSeed {
		t.Fatalf("an older image: %+v, %v", state, err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: "z0-zetta-cluster-ca"}, &corev1.Secret{}); err == nil {
		t.Fatal("a seed was minted for an image that does not read it")
	}
}

// A cluster whose nodes generated their CA under zetta PR #1512 gets no seed (it would stop the
// node); the key is read from node 0's join command once it runs an image that prints it.
func TestARunningClusterLearnsTheCAItsNodesHold(t *testing.T) {
	ctx := context.Background()
	z := newZettaTestCluster()
	z.Spec.Image = "z:1514"
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: z.Namespace}}
	node := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta-0", Namespace: z.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "zetta", Image: "z:1514"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "zetta", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	r, cl := identityTestReconciler(t, z, adminSecretFor(z, "old-token"), sts, node)
	r.PodLogs = func(_ context.Context, _ string, pod string) (string, error) {
		if pod != "z0-zetta-0" {
			t.Fatalf("read the log of %s", pod)
		}
		return "zetta: join with: zetta --peer 10.0.0.1:7777 --join-token <token> --identity-root ff --cluster-ca " + testZettaCAPublic + "\n", nil
	}
	state, err := r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1514", testZettaUsageEnrollment), "z0-zetta", "z0-zetta-headless")
	if err != nil {
		t.Fatal(err)
	}
	var secret corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: "z0-zetta-cluster-ca"}, &secret); err != nil {
		t.Fatal(err)
	}
	if _, minted := secret.Data["seed"]; minted || string(secret.Data["public"]) != testZettaCAPublic {
		t.Fatalf("CA Secret = %v", secret.Data)
	}
	if state.render.caSeed || state.render.clusterCA != testZettaCAPublic || state.status.ClusterCASource != "Members" {
		t.Fatalf("render = %+v, status = %+v", state.render, state.status)
	}
}

// A burst slice never holds the CA: it is told the key of a permanent slice of its cluster.
func TestABurstSliceEnrollsThroughItsPermanentSibling(t *testing.T) {
	ctx := context.Background()
	core := newZettaTestCluster()
	core.Spec.ClusterName = "global"
	burst := newZettaTestCluster()
	burst.Name, burst.UID = "z1", "uid-z1"
	burst.Spec.ClusterName, burst.Spec.Ephemeral, burst.Spec.Image = "global", true, "z:1514"
	caSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta-cluster-ca", Namespace: core.Namespace},
		Data: map[string][]byte{"public": []byte(testZettaCAPublic)}}
	scheme := zettaTestScheme(t)
	if err := controllerutil.SetControllerReference(core, caSecret, scheme); err != nil {
		t.Fatal(err)
	}
	r, _ := identityTestReconciler(t, core, burst, caSecret, adminSecretFor(burst, "burst-token"))
	state, err := r.reconcileZettaIdentity(ctx, burst, identityFeatures("z:1514", testZettaUsageEnrollment), "z1-zetta", "z1-zetta-headless")
	if err != nil {
		t.Fatal(err)
	}
	if state.render.caSeed || state.render.clusterCA != testZettaCAPublic || state.status.ClusterCASource != "Sibling" {
		t.Fatalf("render = %+v, status = %+v", state.render, state.status)
	}
}

// operator#1174 item 1: the identity root moves on node 0, with the current token, before any pod
// gets the new one; then the new token is the one the pods read and the generation rolls them.
func TestRotatingTheAdminTokenMovesTheIdentityRootFirst(t *testing.T) {
	ctx := context.Background()
	z := newZettaTestCluster()
	z.Spec.Image = "z:1512"
	z.Spec.Identity.AdminTokenGeneration = 2
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: z.Namespace, Generation: 4},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 4, ReadyReplicas: 3, UpdatedReplicas: 3, CurrentRevision: "a", UpdateRevision: "a"}}
	r, cl := identityTestReconciler(t, z, adminSecretFor(z, "old-token"), sts)

	var asked struct {
		bearer string
		next   string
	}
	answer := `{"ok":true,"value":{"old_root":"aa","new_root":"bb","resealed":3,"revoked":0,"owner_token_id":"t"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/rotate_identity_root" {
			t.Errorf("%s %s", req.Method, req.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		asked.bearer = req.Header.Get("Authorization")
		asked.next, _ = body["new_admin_token"].(string)
		if revoke, set := body["revoke_tokens"]; set && revoke != false {
			t.Error("a rotation never revokes the tokens other holders of the Secret use")
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	r.ZettaNodeURL = func(*oramav1alpha1.OramaZettaCluster) string { return server.URL }

	// An image without rotate_identity_root waits and touches nothing.
	state, err := r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1512", testZettaUsageWithFile), "z0-zetta", "z0-zetta-headless")
	if err != nil || state.condition == nil || state.condition.Reason != "ImageNotSupported" || asked.bearer != "" || state.render.adminGeneration != 1 {
		t.Fatalf("an older image: %+v, %v", state, err)
	}

	state, err = r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1512", testZettaUsageIdentity), "z0-zetta", "z0-zetta-headless")
	if err != nil {
		t.Fatal(err)
	}
	if asked.bearer != "Bearer old-token" || len(asked.next) != 64 {
		t.Fatalf("rotate_identity_root was called with %q for %d characters", asked.bearer, len(asked.next))
	}
	var admin corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: "z0-zetta-admin"}, &admin); err != nil {
		t.Fatal(err)
	}
	if string(admin.Data["token"]) != asked.next || string(admin.Data["generation"]) != "2" {
		t.Fatalf("admin Secret after: token moved %v, generation %q", string(admin.Data["token"]) == asked.next, admin.Data["generation"])
	}
	if _, left := admin.Data["next"]; left {
		t.Fatal("the pending token is left in the Secret")
	}
	if state.render.adminGeneration != 2 || state.status.AdminTokenGeneration != 2 || state.condition.Reason != "Rotated" {
		t.Fatalf("state = %+v", state)
	}
	rolled := renderZettaWithIdentity(t, z, state.render)
	if rolled.Spec.Template.Annotations[zettaAdminGenerationAnnotation] != "2" {
		t.Fatal("the new token does not roll the nodes")
	}
	if plain := renderZettaWithIdentity(t, newZettaTestCluster(), zettaIdentityRender{adminGeneration: 1}); plain.Spec.Template.Annotations[zettaAdminGenerationAnnotation] != "" {
		t.Fatal("a cluster that never rotated got the annotation")
	}

	// A second raise whose call already went through on node 0 (the operator stopped before it
	// recorded it): node 0 says the token derives its root, which counts as done.
	z.Spec.Identity.AdminTokenGeneration = 3
	asked.bearer = ""
	answer = `{"ok":false,"error":{"code":"conflict","message":"the new admin token derives the current root"}}`
	state, err = r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1512", testZettaUsageIdentity), "z0-zetta", "z0-zetta-headless")
	if err != nil || state.condition.Reason != "Rotated" || state.render.adminGeneration != 3 {
		t.Fatalf("an already moved root: %+v, %v", state, err)
	}

	// Any other refusal leaves the token the pods read alone.
	z.Spec.Identity.AdminTokenGeneration = 4
	answer = `{"ok":false,"error":{"code":"conflict","message":"this node does not hold the current identity root"}}`
	_ = cl.Get(ctx, client.ObjectKeyFromObject(&admin), &admin)
	before := string(admin.Data["token"])
	state, err = r.reconcileZettaIdentity(ctx, z, identityFeatures("z:1512", testZettaUsageIdentity), "z0-zetta", "z0-zetta-headless")
	_ = cl.Get(ctx, client.ObjectKeyFromObject(&admin), &admin)
	if err != nil || state.condition.Reason != "RotationFailed" || string(admin.Data["token"]) != before || state.render.adminGeneration != 3 {
		t.Fatalf("a refused rotation: %+v, %v", state, err)
	}
	if strings.Contains(state.condition.Message, before) || strings.Contains(state.condition.Message, string(admin.Data["next"])) {
		t.Fatal("a token is in the condition")
	}
}

// A rotation waits for every node on the current token, and never runs on a multi-slice cluster.
func TestARotationWaitsForSettledNodesOnOneSlice(t *testing.T) {
	ctx := context.Background()
	features := identityFeatures("z:1512", testZettaUsageIdentity)
	z := newZettaTestCluster()
	z.Spec.Image = "z:1512"
	z.Spec.Identity.AdminTokenGeneration = 2
	rolling := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: z.Namespace},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: 2, UpdatedReplicas: 3, CurrentRevision: "a", UpdateRevision: "a"}}
	r, _ := identityTestReconciler(t, z, adminSecretFor(z, "old-token"), rolling)
	r.ZettaNodeURL = func(*oramav1alpha1.OramaZettaCluster) string {
		t.Fatal("rotate_identity_root called while a node was not ready")
		return ""
	}
	if state, _ := r.reconcileZettaIdentity(ctx, z, features, "z0-zetta", "z0-zetta-headless"); state.condition.Reason != "WaitingForNodes" {
		t.Fatalf("condition = %+v", state.condition)
	}
	z.Spec.ExternalPeers = []string{"peer.example.com:7777"}
	if state, _ := r.reconcileZettaIdentity(ctx, z, features, "z0-zetta", "z0-zetta-headless"); state.condition.Reason != "MultiSliceNotSupported" {
		t.Fatalf("condition = %+v", state.condition)
	}
	z.Spec.Identity.AdminTokenGeneration = 0
	if state, _ := r.reconcileZettaIdentity(ctx, z, features, "z0-zetta", "z0-zetta-headless"); state.condition != nil {
		t.Fatal("a cluster that asks for no rotation carries the condition")
	}
}

// The enrollment variables are the operator's.
func TestSpecEnvCannotSetTheEnrollmentCA(t *testing.T) {
	_, ignored := zettaCustomEnv([]corev1.EnvVar{{Name: "ZETTA_CLUSTER_CA", Value: testZettaCAPublic},
		{Name: "ZETTA_CLUSTER_CA_SEED_FILE", Value: "/data/seed"}})
	if len(ignored) != 2 {
		t.Fatalf("ignored = %v", ignored)
	}
}
