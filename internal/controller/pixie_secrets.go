/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Secrets and certificates of the pixie provider (operator#15). Every one is minted by the
// operator on the cluster that uses it, never copied from another cluster: a value is generated
// once and never rewritten (rotating Pixie's signing keys would sign every user and cluster out),
// and certificates are reissued by the operator before they expire, which rolls the pods that read
// them (pixieCertsAnnotation).

// pixieSecretKey generates one key's value the first time it is missing.
type pixieSecretKey struct {
	Name     string
	Generate func(existing map[string][]byte) (string, error)
}

func randomKey(name string, nbytes int) pixieSecretKey {
	return pixieSecretKey{Name: name, Generate: func(map[string][]byte) (string, error) { return randHex(nbytes) }}
}

func fixedKey(name, value string) pixieSecretKey {
	return pixieSecretKey{Name: name, Generate: func(map[string][]byte) (string, error) { return value, nil }}
}

// bcryptOf hashes another key of the same Secret, for a store that keeps only hashes.
func bcryptOf(name, of string) pixieSecretKey {
	return pixieSecretKey{Name: name, Generate: func(existing map[string][]byte) (string, error) {
		hash, err := bcrypt.GenerateFromPassword(existing[of], 12)
		return string(hash), err
	}}
}

// ensureMintedSecret creates the Secret, or adds the keys it lacks, in order (a later key may be
// derived from an earlier one). A key that exists is never changed, so a steady cluster writes
// nothing. A Secret of the same name that another owner controls is refused.
func ensureMintedSecret(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	name string,
	secretType corev1.SecretType,
	labels map[string]string,
	keys ...pixieSecretKey,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, owner) {
			return fmt.Errorf("Secret %s/%s exists and is not controlled by %s", secret.Namespace, name, owner.GetName())
		}
		if secret.ResourceVersion == "" {
			secret.Type = secretType
		}
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		for key, value := range labels {
			secret.Labels[key] = value
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for _, key := range keys {
			if _, ok := secret.Data[key.Name]; ok {
				continue
			}
			value, err := key.Generate(secret.Data)
			if err != nil {
				return fmt.Errorf("generate %s/%s %s: %w", secret.Namespace, name, key.Name, err)
			}
			secret.Data[key.Name] = []byte(value)
		}
		return controllerutil.SetControllerReference(owner, secret, scheme)
	})
	return secret, err
}

// pixieLeaf is one certificate the operator issues from a CA it keeps.
type pixieLeaf struct {
	Secret  string
	Request tunnelLeafRequest
	// CertKey and KeyKey name the Secret's keys; CAKey, when set, also carries the CA certificate.
	CertKey, KeyKey, CAKey string
	// PKCS8 writes the private key as PKCS #8, which OpenSearch requires.
	PKCS8 bool
}

// ensurePixieCA keeps one CA in an operator-only Secret: minted once, never rotated within its ten
// years.
func ensurePixieCA(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, name, commonName string, labels map[string]string, now time.Time) (tunnelCA, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.GetNamespace()}}
	var ca tunnelCA
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, owner) {
			return fmt.Errorf("Secret %s/%s exists and is not controlled by %s", secret.Namespace, name, owner.GetName())
		}
		secret.Labels = copyStringMap(labels)
		ca = tunnelCA{CertPEM: secret.Data["ca.crt"], KeyPEM: secret.Data["ca.key"]}
		if _, _, err := parseTunnelCA(ca); err != nil {
			fresh, err := newTunnelCA(commonName, now)
			if err != nil {
				return err
			}
			ca = fresh
			secret.Data = map[string][]byte{"ca.crt": ca.CertPEM, "ca.key": ca.KeyPEM}
		}
		return controllerutil.SetControllerReference(owner, secret, scheme)
	})
	return ca, err
}

// ensurePixieLeaf keeps one leaf certificate current: issued when missing, expiring, or no longer
// saying what the request says.
func ensurePixieLeaf(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, ca tunnelCA, leaf pixieLeaf, labels map[string]string, now time.Time) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: leaf.Secret, Namespace: owner.GetNamespace()}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		if secret.ResourceVersion != "" && !metav1.IsControlledBy(secret, owner) {
			return fmt.Errorf("Secret %s/%s exists and is not controlled by %s", secret.Namespace, leaf.Secret, owner.GetName())
		}
		secret.Labels = copyStringMap(labels)
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		key := secret.Data[leaf.KeyKey]
		if leaf.PKCS8 {
			key = pkcs8ToSEC1(key)
		}
		current := tunnelLeafCurrent(secret.Data[leaf.CertKey], key, ca.CertPEM, leaf.Request, now)
		if leaf.CAKey != "" && string(secret.Data[leaf.CAKey]) != string(ca.CertPEM) {
			current = false
		}
		if !current {
			cert, key, err := issueTunnelLeaf(ca, leaf.Request, now)
			if err != nil {
				return err
			}
			if leaf.PKCS8 {
				if key, err = sec1ToPKCS8(key); err != nil {
					return err
				}
			}
			// Only this leaf's keys change: Vizier's service-tls-certs holds two leaves.
			secret.Data[leaf.CertKey], secret.Data[leaf.KeyKey] = cert, key
			if leaf.CAKey != "" {
				secret.Data[leaf.CAKey] = ca.CertPEM
			}
		}
		return controllerutil.SetControllerReference(owner, secret, scheme)
	})
	return secret, err
}

func sec1ToPKCS8(sec1 []byte) ([]byte, error) {
	block, _ := pem.Decode(sec1)
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// pkcs8ToSEC1 converts a PKCS #8 EC key back for the currency check, or returns nil.
func pkcs8ToSEC1(pkcs8 []byte) []byte {
	block, _ := pem.Decode(pkcs8)
	if block == nil {
		return nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// secretsHash is a stable digest of the data of some Secrets, for a pod template annotation that
// rolls the pods when any of them changes.
func secretsHash(secrets ...*corev1.Secret) string {
	sum := sha256.New()
	for _, secret := range secrets {
		keys := make([]string, 0, len(secret.Data))
		for key := range secret.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintf(sum, "%s\x00", secret.Name)
		for _, key := range keys {
			fmt.Fprintf(sum, "%s\x00%x\x00", key, secret.Data[key])
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// vizierDNSNames are the names Vizier's services are reached by, as Pixie's cert provisioner
// issues them (src/utils/shared/certs).
func vizierDNSNames(namespace string) []string {
	return []string{
		"*." + namespace + ".svc",
		"*." + namespace + ".svc.cluster.local",
		"*." + namespace + ".pod.cluster.local",
		"pl-nats",
		"localhost",
	}
}

// pixieCloudDNSNames are the names Cloud's services are reached by, as Pixie's cert provisioner
// issues them, with the namespace's .svc names and the search index added.
func pixieCloudDNSNames(namespace string) []string {
	return []string{
		"*." + namespace,
		"*." + namespace + ".svc",
		"*." + namespace + ".svc.cluster.local",
		"*." + namespace + ".pod.cluster.local",
		"*.pl-nats." + namespace + ".svc",
		"*.pl-nats",
		"pl-nats",
		"*.local",
		"localhost",
	}
}
