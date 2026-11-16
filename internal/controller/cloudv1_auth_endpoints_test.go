// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// jwtSecretsHarness is a fake cluster for ensureJWTSecrets.
type jwtSecretsHarness struct {
	t  *testing.T
	oc *oramav1alpha1.OramaCloudV1
	cl client.Client
	r  *OramaCloudV1Reconciler
}

func newJWTSecretsHarness(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *jwtSecretsHarness {
	t.Helper()
	s := ocv1TestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return &jwtSecretsHarness{t: t, oc: newOCV1(nil), cl: cl, r: &OramaCloudV1Reconciler{Client: cl, Scheme: s}}
}

func (h *jwtSecretsHarness) ensure() error {
	return h.r.ensureJWTSecrets(context.Background(), h.oc)
}

// secret reads name, or returns nil when it does not exist.
func (h *jwtSecretsHarness) secret(name string) *corev1.Secret {
	h.t.Helper()
	sec := &corev1.Secret{}
	err := h.cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, sec)
	if client.IgnoreNotFound(err) != nil {
		h.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return sec
}

// checkPublicSecret asserts v1-jwt-pub holds publicKey alone, with the jwt labels and the CR as its
// controller.
func (h *jwtSecretsHarness) checkPublicSecret(publicKey []byte) {
	h.t.Helper()
	pub := h.secret("v1-jwt-pub")
	if pub == nil {
		h.t.Fatal("v1-jwt-pub was not created")
	}
	if !reflect.DeepEqual(pub.Data, map[string][]byte{"JWT_PUBLIC_KEY": publicKey}) || len(pub.StringData) != 0 {
		h.t.Errorf("v1-jwt-pub data has keys %v, want JWT_PUBLIC_KEY alone with the signing pair's value", keysOf(pub.Data))
	}
	if !reflect.DeepEqual(pub.Labels, labelsFor(h.oc, "jwt")) || !metav1.IsControlledBy(pub, h.oc) {
		h.t.Errorf("v1-jwt-pub labels %v, owners %v", pub.Labels, pub.OwnerReferences)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func signingSecret(data map[string]string) *corev1.Secret {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-jwt", Namespace: "orama-system"}, Data: map[string][]byte{}}
	for k, v := range data {
		sec.Data[k] = []byte(v)
	}
	return sec
}

// A fresh stack gets a real pair; deleting the public Secret and reconciling recreates it from the
// signing Secret, which stays as it was; reconciling again changes nothing.
func TestJWTPublicSecretRecoveredWithoutRotation(t *testing.T) {
	h := newJWTSecretsHarness(t, interceptor.Funcs{})
	if err := h.ensure(); err != nil {
		t.Fatal(err)
	}
	jwt := h.secret("v1-jwt")
	privBlock, _ := pem.Decode(jwt.Data["JWT_PRIVATE_KEY"])
	pubBlock, _ := pem.Decode(jwt.Data["JWT_PUBLIC_KEY"])
	if privBlock == nil || pubBlock == nil {
		t.Fatal("the signing Secret does not hold PEM keys")
	}
	priv, err := x509.ParsePKCS1PrivateKey(privBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil || !priv.PublicKey.Equal(pub.(*rsa.PublicKey)) {
		t.Fatalf("the public key is not the private key's (%v)", err)
	}
	h.checkPublicSecret(jwt.Data["JWT_PUBLIC_KEY"])

	if err := h.cl.Delete(context.Background(), h.secret("v1-jwt-pub")); err != nil {
		t.Fatal(err)
	}
	if err := h.ensure(); err != nil {
		t.Fatal(err)
	}
	h.checkPublicSecret(jwt.Data["JWT_PUBLIC_KEY"])
	if got := h.secret("v1-jwt"); got.ResourceVersion != jwt.ResourceVersion || !reflect.DeepEqual(got.Data, jwt.Data) {
		t.Error("recovering the public Secret changed the signing Secret")
	}

	pubVersion := h.secret("v1-jwt-pub").ResourceVersion
	for range 2 {
		if err := h.ensure(); err != nil {
			t.Fatal(err)
		}
	}
	if h.secret("v1-jwt").ResourceVersion != jwt.ResourceVersion || h.secret("v1-jwt-pub").ResourceVersion != pubVersion {
		t.Error("a repeated reconcile rewrote a JWT Secret")
	}
}

// A first run interrupted after the signing Secret was created leaves no public Secret: the next run
// derives it from the existing pair instead of generating a new one. An existing public Secret is
// left as it is.
func TestJWTPublicSecretAfterInterruptedCreation(t *testing.T) {
	h := newJWTSecretsHarness(t, interceptor.Funcs{},
		signingSecret(map[string]string{"JWT_PRIVATE_KEY": "kept-private", "JWT_PUBLIC_KEY": "kept-public"}))
	if err := h.ensure(); err != nil {
		t.Fatal(err)
	}
	h.checkPublicSecret([]byte("kept-public"))
	if got := h.secret("v1-jwt"); string(got.Data["JWT_PRIVATE_KEY"]) != "kept-private" {
		t.Error("the signing pair was replaced")
	}

	// The object just created in this reconcile may carry its keys only under StringData.
	h = newJWTSecretsHarness(t, interceptor.Funcs{})
	fresh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-jwt", Namespace: "orama-system"},
		StringData: map[string]string{"JWT_PRIVATE_KEY": "fresh-private", "JWT_PUBLIC_KEY": "fresh-public"}}
	if err := h.r.ensureJWTPublicSecret(context.Background(), h.oc, fresh); err != nil {
		t.Fatal(err)
	}
	h.checkPublicSecret([]byte("fresh-public"))

	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-jwt-pub", Namespace: "orama-system"},
		Data: map[string][]byte{"JWT_PUBLIC_KEY": []byte("someone-elses")}}
	h = newJWTSecretsHarness(t, interceptor.Funcs{},
		signingSecret(map[string]string{"JWT_PRIVATE_KEY": "kept-private", "JWT_PUBLIC_KEY": "kept-public"}), other)
	if err := h.ensure(); err != nil {
		t.Fatal(err)
	}
	if got := h.secret("v1-jwt-pub"); string(got.Data["JWT_PUBLIC_KEY"]) != "someone-elses" {
		t.Error("an existing public Secret was rewritten")
	}
}

// A signing Secret without the public key cannot be recovered from: a readable error, and nothing
// written.
func TestJWTPublicSecretMissingPublicKey(t *testing.T) {
	writes := 0
	count := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return c.Update(ctx, obj, opts...)
		},
	}
	h := newJWTSecretsHarness(t, count, signingSecret(map[string]string{"JWT_PRIVATE_KEY": "kept-private"}))
	err := h.ensure()
	if err == nil || !strings.Contains(err.Error(), "v1-jwt has no JWT_PUBLIC_KEY") {
		t.Fatalf("ensureJWTSecrets error = %v, want one naming the missing JWT_PUBLIC_KEY", err)
	}
	if writes != 0 || h.secret("v1-jwt-pub") != nil {
		t.Errorf("%d writes; the signing Secret must be left as it is and no public Secret created", writes)
	}
}

// When another reconcile creates the signing Secret first, the public Secret comes from the winning
// pair, not the pair this reconcile generated and discarded.
func TestJWTSigningSecretCreationRace(t *testing.T) {
	winner := signingSecret(map[string]string{"JWT_PRIVATE_KEY": "winner-private", "JWT_PUBLIC_KEY": "winner-public"})
	raced := false
	h := newJWTSecretsHarness(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == "v1-jwt" && !raced {
				raced = true
				if err := c.Create(ctx, winner.DeepCopy()); err != nil {
					return err
				}
			}
			return c.Create(ctx, obj, opts...) // AlreadyExists for the losing v1-jwt
		},
	})
	if err := h.ensure(); err != nil {
		t.Fatal(err)
	}
	if !raced {
		t.Fatal("the signing Secret was never created")
	}
	h.checkPublicSecret([]byte("winner-public"))
	if got := h.secret("v1-jwt"); !reflect.DeepEqual(got.Data, winner.Data) {
		t.Error("the winning signing pair was replaced")
	}
}
