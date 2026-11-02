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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// TestEgressGatewayAgainstZettaTunnel runs the real zetta-tunnel egress role with the arguments the
// operator renders and the snapshot the operator writes, and proves #470's and #1136's acceptance
// end to end on loopback: the allowed name reaches its destination; a name not allowed is refused,
// including one that resolves to the allowed name's address; an IP literal is refused; a cluster
// address projected into denied_cidrs is refused; and a removed rule stops authorizing.
//
// The gateway refuses loopback destinations itself (zetta-tunnel#38), so the test loads
// zetta-tunnel's egress_network_fixture into the gateway process: it answers "localhost" with the
// public fixture address 93.184.216.34 and redirects connects to that address to 127.0.0.1. Build
// zetta-tunnel (a commit with #38) and its fixture, then point at both:
//
//	cmake -S . -B build -G Ninja && cmake --build build --target zetta-tunnel egress_network_fixture
//	EGRESS_GATEWAY_BINARY=build/zetta-tunnel EGRESS_GATEWAY_NETWORK_FIXTURE=build/libegress_network_fixture.dylib \
//	  go test ./internal/controller/ -run EgressGatewayAgainstZettaTunnel
//
// (libegress_network_fixture.so on Linux.) The operator's own validation requires a fully qualified
// name, so this test builds the snapshot entry directly.
func TestEgressGatewayAgainstZettaTunnel(t *testing.T) {
	binary := os.Getenv("EGRESS_GATEWAY_BINARY")
	if binary == "" {
		t.Skip("set EGRESS_GATEWAY_BINARY to a zetta-tunnel build to run the egress gateway")
	}
	fixture := os.Getenv("EGRESS_GATEWAY_NETWORK_FIXTURE")
	if fixture == "" {
		t.Fatal("set EGRESS_GATEWAY_NETWORK_FIXTURE to zetta-tunnel's egress_network_fixture library: the gateway refuses loopback destinations")
	}
	// The fixture's public answer for "localhost", which it connects to the local echo server.
	const fixtureAddress = "93.184.216.34"
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
	snapshotDenying := func(inventory ...string) egressGatewaySnapshot {
		t.Helper()
		denied, err := deniedCIDRList(inventory)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, problems := buildEgressGatewaySnapshot("workspace-test", []egressGatewayEntry{{rule: rule, ips: []string{"127.0.0.1"}}}, denied)
		if len(problems) != 0 {
			t.Fatal(problems)
		}
		return snapshot
	}
	// A cluster inventory with a public-numbered node and pod range that are not the destination.
	publish(snapshotDenying("10.0.0.1", "20.237.72.161", "11.244.0.0/24", "::ffff:198.51.99.7"))

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
	loader := "LD_PRELOAD"
	if runtime.GOOS == "darwin" {
		loader = "DYLD_INSERT_LIBRARIES"
	}
	gateway.Env = append(os.Environ(), loader+"="+fixture)
	audit := &syncBuilder{}
	gateway.Stdout = audit
	gateway.Stderr = audit
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

	refusedWithin := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			reply, err := explicit("localhost")
			if err != nil || reply == "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// The destination becomes a cluster address: the same rule, with that address in the inventory,
	// is refused once the gateway reloads, without opening a connection.
	publish(snapshotDenying("10.0.0.1", "20.237.72.161", fixtureAddress, "11.244.0.0/24"))
	refusedWithin("a destination in denied_cidrs is still reachable")
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(audit.String(), `"reason":"destination_forbidden"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the denied destination was not refused as destination_forbidden: %s", audit.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The address leaves the inventory: the name is reachable again. Then the rule leaves the
	// snapshot: the gateway stops authorizing it within its reload interval.
	publish(snapshotDenying("10.0.0.1"))
	deadline = time.Now().Add(10 * time.Second)
	for {
		reply, err := explicit("localhost")
		if err == nil && reply == "hello\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the name did not reach its destination again once the address left denied_cidrs: %q, %v", reply, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	publish(egressGatewaySnapshot{})
	refusedWithin("a removed rule still authorizes its name")
	if !strings.Contains(audit.String(), `"decision":"deny"`) || !strings.Contains(audit.String(), `"decision":"allow"`) {
		t.Fatalf("decision records missing: %s", audit.String())
	}
}

// syncBuilder collects the gateway's output, written by os/exec while the test reads it.
type syncBuilder struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *syncBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(p)
}

func (b *syncBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}
