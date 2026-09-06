/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	sealedSecretVersion = 1
	sealingKDFInfo      = "orama lake sealed secret v1"
)

type sealedSecretAAD struct {
	Cluster   string `cbor:"cluster"`
	Namespace string `cbor:"namespace"`
	Name      string `cbor:"name"`
}

type sealedSecretEnvelope struct {
	Version uint64          `cbor:"v"`
	KID     string          `cbor:"kid"`
	EPK     []byte          `cbor:"epk"`
	Nonce   []byte          `cbor:"nonce"`
	AAD     sealedSecretAAD `cbor:"aad"`
	CT      []byte          `cbor:"ct"`
}

var (
	sealedSecretEncodeMode = mustSealedSecretEncodeMode()
	sealedSecretDecodeMode = mustSealedSecretDecodeMode()
)

func mustSealedSecretEncodeMode() cbor.EncMode {
	mode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}

func mustSealedSecretDecodeMode() cbor.DecMode {
	mode, err := (cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}).DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}

func sealingKID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func decodeSealedSecretRecord(record string) (sealedSecretEnvelope, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(record)
	if err != nil {
		return sealedSecretEnvelope{}, fmt.Errorf("decode base64 record: %w", err)
	}
	envelope := sealedSecretEnvelope{}
	if err := sealedSecretDecodeMode.Unmarshal(raw, &envelope); err != nil {
		return sealedSecretEnvelope{}, fmt.Errorf("decode CBOR envelope: %w", err)
	}
	canonical, err := sealedSecretEncodeMode.Marshal(envelope)
	if err != nil {
		return sealedSecretEnvelope{}, fmt.Errorf("encode canonical CBOR envelope: %w", err)
	}
	if !hmac.Equal(raw, canonical) {
		return sealedSecretEnvelope{}, errors.New("sealed secret envelope is not canonical CBOR")
	}
	if envelope.Version != sealedSecretVersion {
		return sealedSecretEnvelope{}, fmt.Errorf("unsupported sealed secret version %d", envelope.Version)
	}
	if envelope.KID == "" {
		return sealedSecretEnvelope{}, errors.New("sealed secret kid is empty")
	}
	if len(envelope.EPK) != 32 {
		return sealedSecretEnvelope{}, fmt.Errorf("ephemeral public key is %d bytes, want 32", len(envelope.EPK))
	}
	if len(envelope.Nonce) != chacha20poly1305.NonceSizeX {
		return sealedSecretEnvelope{}, fmt.Errorf("nonce is %d bytes, want %d", len(envelope.Nonce), chacha20poly1305.NonceSizeX)
	}
	if len(envelope.CT) < chacha20poly1305.Overhead {
		return sealedSecretEnvelope{}, errors.New("ciphertext is shorter than the authentication tag")
	}
	return envelope, nil
}

func unsealSecretRecord(
	record string,
	privateKey []byte,
	recipientPublicKey []byte,
	expected sealedSecretAAD,
) ([]byte, string, error) {
	envelope, err := decodeSealedSecretRecord(record)
	if err != nil {
		return nil, "", err
	}
	if envelope.AAD != expected {
		return nil, envelope.KID, fmt.Errorf(
			"sealed secret AAD is cluster=%q namespace=%q name=%q, want cluster=%q namespace=%q name=%q",
			envelope.AAD.Cluster,
			envelope.AAD.Namespace,
			envelope.AAD.Name,
			expected.Cluster,
			expected.Namespace,
			expected.Name,
		)
	}
	curve := ecdh.X25519()
	private, err := curve.NewPrivateKey(privateKey)
	if err != nil {
		return nil, envelope.KID, fmt.Errorf("load recipient private key: %w", err)
	}
	ephemeral, err := curve.NewPublicKey(envelope.EPK)
	if err != nil {
		return nil, envelope.KID, fmt.Errorf("load ephemeral public key: %w", err)
	}
	shared, err := private.ECDH(ephemeral)
	if err != nil {
		return nil, envelope.KID, fmt.Errorf("derive X25519 shared secret: %w", err)
	}
	key, err := deriveSealedSecretKey(shared, envelope.EPK, recipientPublicKey)
	if err != nil {
		return nil, envelope.KID, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, envelope.KID, fmt.Errorf("create XChaCha20-Poly1305: %w", err)
	}
	aad, err := sealedSecretEncodeMode.Marshal(envelope.AAD)
	if err != nil {
		return nil, envelope.KID, fmt.Errorf("encode envelope AAD: %w", err)
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.CT, aad)
	if err != nil {
		return nil, envelope.KID, errors.New("sealed secret authentication failed")
	}
	return plaintext, envelope.KID, nil
}

func deriveSealedSecretKey(shared []byte, ephemeralPublicKey []byte, recipientPublicKey []byte) ([]byte, error) {
	info := make([]byte, 0, len(sealingKDFInfo)+len(ephemeralPublicKey)+len(recipientPublicKey))
	info = append(info, sealingKDFInfo...)
	info = append(info, ephemeralPublicKey...)
	info = append(info, recipientPublicKey...)
	prkMAC := hmac.New(sha256.New, make([]byte, sha256.Size))
	if _, err := prkMAC.Write(shared); err != nil {
		return nil, err
	}
	prk := prkMAC.Sum(nil)
	expandMAC := hmac.New(sha256.New, prk)
	if _, err := expandMAC.Write(info); err != nil {
		return nil, err
	}
	if _, err := expandMAC.Write([]byte{1}); err != nil {
		return nil, err
	}
	return expandMAC.Sum(nil), nil
}
