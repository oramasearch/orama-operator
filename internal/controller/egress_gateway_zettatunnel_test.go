/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestEgressGatewayAgainstZettaTunnel runs the real zetta-tunnel egress role with the arguments the
// operator renders and the snapshot the operator writes, and proves #470's acceptance end to end on
// loopback: the allowed name reaches its destination; a name not allowed is refused, including one
// that resolves to the allowed name's address; an IP literal is refused; and a removed rule stops
// authorizing. Build zetta-tunnel at the image's commit (933d6fc) and point at the binary:
//
//	EGRESS_GATEWAY_BINARY=/path/to/zetta-tunnel go test ./internal/controller/ -run EgressGatewayAgainstZettaTunnel
//
// The loopback name is "localhost" because the gateway resolves names itself; the operator's own
// validation requires a fully qualified name, so this test builds the snapshot entry directly.
func TestEgressGatewayAgainstZettaTunnel(t *testing.T) {
	binary := os.Getenv("EGRESS_GATEWAY_BINARY")
	if binary == "" {
		t.Skip("set EGRESS_GATEWAY_BINARY to a zetta-tunnel build to run the egress gateway")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			connection, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	destinationPort := int32(echo.Addr().(*net.TCPAddr).Port)

	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.json")
	publish := func(snapshot egressGatewaySnapshot) {
		t.Helper()
		encoded, err := encodeEgressGatewaySnapshot(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		// Atomic replacement, as the kubelet does for the ConfigMap volume.
		next := filepath.Join(dir, "rules.next")
		if err := os.WriteFile(next, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, rulesPath); err != nil {
			t.Fatal(err)
		}
	}
	rule := &oramav1alpha1.EgressRule{
		ObjectMeta: metav1.ObjectMeta{Name: lakeEgressRuleName(testEgressNameRuleID)},
		Spec:       oramav1alpha1.EgressRuleSpec{RuleID: testEgressNameRuleID, DNSName: "localhost", Ports: []int32{destinationPort}},
	}
	snapshot, problems := buildEgressGatewaySnapshot("workspace-test", []egressGatewayEntry{{rule: rule, ips: []string{"127.0.0.1"}}})
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	publish(snapshot)

	freePort := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}
	listen, health := freePort(), freePort()
	args := egressGatewayDeploymentSpec(&oramav1alpha1.Lake{}).Template.Spec.Containers[0].Args
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--rules":
			args[i+1] = rulesPath
		case "--listen":
			args[i+1] = fmt.Sprintf("127.0.0.1:%d", listen)
		case "--health-listen":
			args[i+1] = fmt.Sprintf("127.0.0.1:%d", health)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway := exec.CommandContext(ctx, binary, args...)
	var audit strings.Builder
	gateway.Stdout = &audit
	gateway.Stderr = &audit
	if err := gateway.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = gateway.Wait()
		if t.Failed() {
			t.Log(audit.String())
		}
	}()
	ready := func() bool {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", health))
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	}
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("the gateway did not become ready with the operator's arguments and snapshot")
		}
		time.Sleep(100 * time.Millisecond)
	}

	explicit := func(name string) (string, error) {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listen), 5*time.Second)
		if err != nil {
			return "", err
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(connection, "ZTEG1 %s %d\nhello\n", name, destinationPort); err != nil {
			return "", err
		}
		return bufio.NewReader(connection).ReadString('\n')
	}
	if reply, err := explicit("localhost"); err != nil || reply != "hello\n" {
		t.Fatalf("the allowed name did not reach its destination: %q, %v", reply, err)
	}
	for _, refused := range []string{"not-allowed.localhost", "127.0.0.1", "[::1]"} {
		if reply, err := explicit(refused); err == nil || reply != "" {
			t.Fatalf("%s was not refused: %q, %v", refused, reply, err)
		}
	}

	// The rule leaves the snapshot: the gateway stops authorizing it within its reload interval.
	publish(egressGatewaySnapshot{})
	deadline = time.Now().Add(10 * time.Second)
	for {
		reply, err := explicit("localhost")
		if err != nil || reply == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a removed rule still authorizes its name")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !strings.Contains(audit.String(), `"decision":"deny"`) || !strings.Contains(audit.String(), `"decision":"allow"`) {
		t.Fatalf("decision records missing: %s", audit.String())
	}
}
