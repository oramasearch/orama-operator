/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// Package escrow seals a key to recovery keys held off the cluster (operator#739), in the age format
// (age-encryption.org/v1, X25519 recipients), so the person holding a recovery key opens it with the
// standard `age` tool and nothing of ours: `age -d -i recovery.key <file>`. It writes the format from
// its specification (https://age-encryption.org/v1) on golang.org/x/crypto, which the operator already
// depends on.
package escrow

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	ageIntro       = "age-encryption.org/v1"
	ageX25519Label = "age-encryption.org/v1/X25519"
	ageChunkSize   = 64 << 10
	recipientHRP   = "age"
	identityHRP    = "age-secret-key-"
)

var b64 = base64.RawStdEncoding

// ParseRecipient reads an age X25519 recipient, "age1...", and returns its public key.
func ParseRecipient(text string) ([]byte, error) {
	hrp, data, err := bech32Decode(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("recipient %q: %w", text, err)
	}
	if hrp != recipientHRP || len(data) != curve25519.PointSize {
		return nil, fmt.Errorf("recipient %q is not an age X25519 public key", text)
	}
	return data, nil
}

// Recipient formats an X25519 public key as "age1...".
func Recipient(public []byte) (string, error) { return bech32Encode(recipientHRP, public) }

// NewIdentity makes a new X25519 identity: its secret "AGE-SECRET-KEY-1..." and its recipient.
// Only tests and rehearsals use it; a real recovery key is made by its holder, off the cluster.
func NewIdentity() (string, string, error) {
	secret := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	public, err := curve25519.X25519(secret, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	identity, err := bech32Encode(identityHRP, secret)
	if err != nil {
		return "", "", err
	}
	recipient, err := Recipient(public)
	return strings.ToUpper(identity), recipient, err
}

// Encrypt seals plaintext to every recipient: any one of their identities opens it.
func Encrypt(recipients []string, plaintext []byte) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("no recipient")
	}
	fileKey := make([]byte, 16)
	if _, err := rand.Read(fileKey); err != nil {
		return nil, err
	}
	var header bytes.Buffer
	header.WriteString(ageIntro + "\n")
	for _, text := range recipients {
		public, err := ParseRecipient(text)
		if err != nil {
			return nil, err
		}
		ephemeral := make([]byte, curve25519.ScalarSize)
		if _, err := rand.Read(ephemeral); err != nil {
			return nil, err
		}
		share, err := curve25519.X25519(ephemeral, curve25519.Basepoint)
		if err != nil {
			return nil, err
		}
		shared, err := curve25519.X25519(ephemeral, public)
		if err != nil {
			return nil, err
		}
		wrapKey, err := deriveKey(shared, append(append([]byte{}, share...), public...), ageX25519Label)
		if err != nil {
			return nil, err
		}
		body, err := seal(wrapKey, make([]byte, chacha20poly1305.NonceSize), fileKey)
		if err != nil {
			return nil, err
		}
		header.WriteString("-> X25519 " + b64.EncodeToString(share) + "\n")
		writeWrapped(&header, b64.EncodeToString(body))
	}
	header.WriteString("---")
	macKey, err := deriveKey(fileKey, nil, "header")
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(header.Bytes())
	header.WriteString(" " + b64.EncodeToString(mac.Sum(nil)) + "\n")

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	payloadKey, err := deriveKey(fileKey, nonce, "payload")
	if err != nil {
		return nil, err
	}
	out := append(header.Bytes(), nonce...)
	for counter := uint64(0); ; counter++ {
		chunk := plaintext
		last := len(chunk) <= ageChunkSize
		if !last {
			chunk = plaintext[:ageChunkSize]
		}
		sealed, err := seal(payloadKey, streamNonce(counter, last), chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, sealed...)
		if last {
			return out, nil
		}
		plaintext = plaintext[ageChunkSize:]
	}
}

// Decrypt opens a file sealed to the identity "AGE-SECRET-KEY-1...". The operator never holds a
// recovery identity; tests and the rehearsal tool use this to check what Encrypt wrote.
func Decrypt(identity string, ciphertext []byte) ([]byte, error) {
	hrp, secret, err := bech32Decode(strings.ToLower(strings.TrimSpace(identity)))
	if err != nil || hrp != identityHRP || len(secret) != curve25519.ScalarSize {
		return nil, errors.New("not an age X25519 identity")
	}
	public, err := curve25519.X25519(secret, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	end := bytes.Index(ciphertext, []byte("\n---"))
	if !bytes.HasPrefix(ciphertext, []byte(ageIntro+"\n")) || end < 0 {
		return nil, errors.New("not an age v1 file")
	}
	headerEnd := end + len("\n---")
	macLineEnd := bytes.IndexByte(ciphertext[headerEnd:], '\n')
	if macLineEnd < 0 {
		return nil, errors.New("age header has no MAC line")
	}
	macText := strings.TrimPrefix(string(ciphertext[headerEnd:headerEnd+macLineEnd]), " ")
	lines := strings.Split(string(ciphertext[len(ageIntro)+1:end+1]), "\n")
	var fileKey []byte
	for i := 0; i < len(lines); i++ {
		fields := strings.Fields(lines[i])
		if len(fields) != 3 || fields[0] != "->" || fields[1] != "X25519" {
			continue
		}
		var body strings.Builder
		for i++; i < len(lines); i++ {
			body.WriteString(lines[i])
			if len(lines[i]) < 64 {
				break
			}
		}
		share, err1 := b64.DecodeString(fields[2])
		sealed, err2 := b64.DecodeString(body.String())
		if err1 != nil || err2 != nil {
			continue
		}
		shared, err := curve25519.X25519(secret, share)
		if err != nil {
			continue
		}
		wrapKey, err := deriveKey(shared, append(append([]byte{}, share...), public...), ageX25519Label)
		if err != nil {
			return nil, err
		}
		if key, err := open(wrapKey, make([]byte, chacha20poly1305.NonceSize), sealed); err == nil {
			fileKey = key
			break
		}
	}
	if fileKey == nil {
		return nil, errors.New("no recipient stanza opens with this identity")
	}
	macKey, err := deriveKey(fileKey, nil, "header")
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(ciphertext[:headerEnd])
	if want, err := b64.DecodeString(macText); err != nil || !hmac.Equal(want, mac.Sum(nil)) {
		return nil, errors.New("age header MAC does not match")
	}
	payload := ciphertext[headerEnd+macLineEnd+1:]
	if len(payload) < 16 {
		return nil, errors.New("age payload has no nonce")
	}
	payloadKey, err := deriveKey(fileKey, payload[:16], "payload")
	if err != nil {
		return nil, err
	}
	payload = payload[16:]
	var plaintext []byte
	sealedChunk := ageChunkSize + chacha20poly1305.Overhead
	for counter := uint64(0); ; counter++ {
		chunk := payload
		last := len(chunk) <= sealedChunk
		if !last {
			chunk = payload[:sealedChunk]
		}
		opened, err := open(payloadKey, streamNonce(counter, last), chunk)
		if err != nil {
			return nil, fmt.Errorf("age payload chunk %d: %w", counter, err)
		}
		plaintext = append(plaintext, opened...)
		if last {
			return plaintext, nil
		}
		payload = payload[sealedChunk:]
	}
}

func deriveKey(secret, salt []byte, info string) ([]byte, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	_, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, []byte(info)), key)
	return key, err
}

func seal(key, nonce, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, nil), nil
}

func open(key, nonce, sealed []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, sealed, nil)
}

// streamNonce is the STREAM nonce: an 11-byte big-endian counter and the last-chunk flag.
func streamNonce(counter uint64, last bool) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	for i := 10; i >= 3; i-- {
		nonce[i] = byte(counter)
		counter >>= 8
	}
	if last {
		nonce[11] = 1
	}
	return nonce
}

// writeWrapped writes base64 in lines of 64 columns; the last line is always shorter than 64, so a
// body that fills its last line exactly is followed by an empty one.
func writeWrapped(w *bytes.Buffer, text string) {
	for len(text) >= 64 {
		w.WriteString(text[:64] + "\n")
		text = text[64:]
	}
	w.WriteString(text + "\n")
}

// Bech32 (BIP 173), as age encodes its keys.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	generator := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	checksum := uint32(1)
	for _, v := range values {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				checksum ^= generator[i]
			}
		}
	}
	return checksum
}

func bech32Expand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc, bits uint
	var out []byte
	maxValue := uint(1)<<to - 1
	for _, b := range data {
		if uint(b)>>from != 0 {
			return nil, errors.New("bech32: invalid data")
		}
		acc = acc<<from | uint(b)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxValue))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxValue))
		}
	} else if bits >= from || acc<<(to-bits)&maxValue != 0 {
		return nil, errors.New("bech32: invalid padding")
	}
	return out, nil
}

func bech32Encode(hrp string, data []byte) (string, error) {
	values, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	polymod := bech32Polymod(append(append(bech32Expand(hrp), values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var out strings.Builder
	out.WriteString(hrp + "1")
	for _, v := range values {
		out.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		out.WriteByte(bech32Charset[(polymod>>uint(5*(5-i)))&31])
	}
	return out.String(), nil
}

func bech32Decode(text string) (string, []byte, error) {
	if strings.ToLower(text) != text && strings.ToUpper(text) != text {
		return "", nil, errors.New("bech32: mixed case")
	}
	text = strings.ToLower(text)
	separator := strings.LastIndexByte(text, '1')
	if separator < 1 || separator+7 > len(text) {
		return "", nil, errors.New("bech32: no separator")
	}
	hrp := text[:separator]
	var values []byte
	for i := separator + 1; i < len(text); i++ {
		index := strings.IndexByte(bech32Charset, text[i])
		if index < 0 {
			return "", nil, errors.New("bech32: invalid character")
		}
		values = append(values, byte(index))
	}
	if bech32Polymod(append(bech32Expand(hrp), values...)) != 1 {
		return "", nil, errors.New("bech32: checksum")
	}
	data, err := convertBits(values[:len(values)-6], 5, 8, false)
	return hrp, data, err
}
