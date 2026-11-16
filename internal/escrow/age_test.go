/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package escrow

import (
	"bytes"
	"strings"
	"testing"
)

func TestBech32ReadsTheBIP173Vectors(t *testing.T) {
	if hrp, data, err := bech32Decode("a12uel5l"); err != nil || hrp != "a" || len(data) != 0 {
		t.Fatalf("a12uel5l: %q %v %v", hrp, data, err)
	}
	encoded, _ := bech32Encode("age", bytes.Repeat([]byte{7}, 32))
	if hrp, data, err := bech32Decode(encoded); err != nil || hrp != "age" || !bytes.Equal(data, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("round trip %s: %v", encoded, err)
	}
	if _, _, err := bech32Decode("a12uel5m"); err == nil {
		t.Fatal("a bad checksum was accepted")
	}
}

// What one recipient's identity opens is what was sealed, across several payload chunks; another
// identity opens nothing; a changed byte is refused.
func TestEncryptOpensWithAnyRecipientsIdentity(t *testing.T) {
	first, firstRecipient, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, secondRecipient, _ := NewIdentity()
	stranger, _, _ := NewIdentity()
	if !strings.HasPrefix(first, "AGE-SECRET-KEY-1") || !strings.HasPrefix(firstRecipient, "age1") || len(firstRecipient) != 62 {
		t.Fatalf("identity %s recipient %s", first[:16], firstRecipient)
	}
	plaintext := bytes.Repeat([]byte("instance-kek "), 20000) // more than two 64 KiB chunks
	sealed, err := Encrypt([]string{firstRecipient, secondRecipient}, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte("age-encryption.org/v1\n-> X25519 ")) {
		t.Fatalf("header = %q", sealed[:40])
	}
	for _, identity := range []string{first, second} {
		opened, err := Decrypt(identity, sealed)
		if err != nil || !bytes.Equal(opened, plaintext) {
			t.Fatalf("decrypt: %v", err)
		}
	}
	if _, err := Decrypt(stranger, sealed); err == nil {
		t.Fatal("a stranger's identity opened it")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := Decrypt(first, tampered); err == nil {
		t.Fatal("a changed byte was accepted")
	}
	if _, err := Encrypt([]string{"age1notarecipient"}, plaintext); err == nil {
		t.Fatal("a malformed recipient was accepted")
	}
}
