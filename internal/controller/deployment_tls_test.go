/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDeploymentTestCertificate(t *testing.T, directory string, serial int64) ([]byte, string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "deployment-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certFile, keyFile := filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key")
	if err := os.WriteFile(certFile, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certificate, certFile, keyFile
}

func TestDeploymentTLSRequiresCertificateAndReloadsRenewal(t *testing.T) {
	if err := (&DeploymentControlPlane{BindAddress: "127.0.0.1:0"}).Start(context.Background()); err == nil {
		t.Fatal("listener started without TLS credentials")
	}
	directory := t.TempDir()
	certificate, certFile, keyFile := writeDeploymentTestCertificate(t, directory, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	controlPlane := &DeploymentControlPlane{BindAddress: address, CertFile: certFile, KeyFile: keyFile}
	config, err := controlPlane.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS12 {
		t.Fatal("TLS accepts obsolete protocol versions")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controlPlane.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var response *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get("https://" + address + "/v1/openapi.json")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil {
		t.Fatal("verified HTTPS did not serve the contract")
	}
	response, err = client.Get("http://" + address + "/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatal("plaintext request reached the control-plane handler")
	}
	// Updating the projected pair changes the next handshake without restarting the server.
	renewed, _, _ := writeDeploymentTestCertificate(t, directory, 2)
	roots.AppendCertsFromPEM(renewed)
	response, err = client.Get("https://" + address + "/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.TLS.PeerCertificates[0].SerialNumber.Int64() != 2 {
		t.Fatal("listener kept serving the old certificate after renewal")
	}
	if err := os.WriteFile(keyFile, []byte("invalid test key"), 0600); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Get("https://" + address + "/v1/openapi.json"); err == nil {
		response.Body.Close()
		t.Fatal("listener accepted an invalid renewed key")
	}
}
