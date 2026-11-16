// SPDX-License-Identifier: MIT

package meshprojection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeProc(t *testing.T, root string, pid string, cgroup string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, pid, "cgroup"), []byte(cgroup), 0o444); err != nil {
		t.Fatal(err)
	}
}

// writeNetNamespace stands in for /proc/<pid>/ns/net: a file whose inode the resolver reads.
func writeNetNamespace(t *testing.T, root string, pid string) uint64 {
	t.Helper()
	path := filepath.Join(root, pid, "ns", "net")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func TestProcResolverFindsTheSandboxProcessOfAPod(t *testing.T) {
	root := t.TempDir()
	const systemd = "1f0e3dad-9990-4a2b-8c2d-000000000001"
	const cgroupfs = "1f0e3dad-9990-4a2b-8c2d-000000000002"
	writeProc(t, root, "1", "0::/init.scope\n")
	writeProc(t, root, "4201", "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1f0e3dad_9990_4a2b_8c2d_000000000001.slice/cri-containerd-b.scope\n")
	writeProc(t, root, "4100", "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1f0e3dad_9990_4a2b_8c2d_000000000001.slice/cri-containerd-a.scope\n")
	writeProc(t, root, "5000", "0::/kubepods/besteffort/pod"+cgroupfs+"/c\n")
	systemdInode := writeNetNamespace(t, root, "4100")
	writeNetNamespace(t, root, "4201")
	cgroupfsInode := writeNetNamespace(t, root, "5000")
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolve, err := ProcResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if namespace, ok := resolve(systemd); !ok || namespace.Path != filepath.Join(root, "4100", "ns", "net") || namespace.Inode != systemdInode {
		t.Fatalf("systemd pod = %+v %v, want the lowest PID and its namespace's inode", namespace, ok)
	}
	if namespace, ok := resolve(cgroupfs); !ok || namespace.Path != filepath.Join(root, "5000", "ns", "net") || namespace.Inode != cgroupfsInode {
		t.Fatalf("cgroupfs pod = %+v %v", namespace, ok)
	}
	if _, ok := resolve("00000000-0000-4000-8000-000000000000"); ok {
		t.Fatal("a pod with no process here was resolved")
	}
	if _, ok := resolve(""); ok {
		t.Fatal("an empty UID was resolved")
	}
}

// operator#869: membership is an exact pod component, never a prefix or a second pod; a process
// without a namespace to pin is skipped for the next one.
func TestProcResolverRequiresExactPodMembership(t *testing.T) {
	const uid = "1f0e3dad-9990-4a2b-8c2d-000000000003"
	for cgroup, member := range map[string]bool{
		"0::/kubepods/besteffort/pod" + uid + "/c\n":                                          true,
		"0::/kubepods.slice/kubepods-pod1f0e3dad_9990_4a2b_8c2d_000000000003.slice/x.scope\n": true,
		"0::/kubepods/besteffort/pod" + uid + "0/c\n":                                         false,
		"0::/kubepods/besteffort/pod1f0e3dad-9990-4a2b-8c2d-00000000000/c\n":                  false,
		"0::/kubepods/pod" + uid + "/pod1f0e3dad-9990-4a2b-8c2d-000000000004/c\n":             false,
		"0::/init.scope\n": false,
		"garbage\n":        false,
	} {
		if CgroupNamesPod(cgroup, uid) != member {
			t.Errorf("%q: member = %v", cgroup, !member)
		}
	}
	root := t.TempDir()
	writeProc(t, root, "10", "0::/kubepods/besteffort/pod"+uid+"/c\n") // gone between read and pin: no ns/net
	writeProc(t, root, "11", "0::/kubepods/besteffort/pod"+uid+"/c\n")
	inode := writeNetNamespace(t, root, "11")
	resolve, err := ProcResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if namespace, ok := resolve(uid); !ok || namespace.Path != filepath.Join(root, "11", "ns", "net") || namespace.Inode != inode {
		t.Fatalf("resolved %+v %v, want PID 11", namespace, ok)
	}
	if _, ok := resolve(strings.ToUpper(uid)); ok {
		t.Fatal("an upper-case UID was resolved")
	}
}

func TestRenderLeavesOutPodsNotOnTheNodeAndWritesZettaTunnelsShape(t *testing.T) {
	template := Template{Node: "n1", Workloads: []TemplateWorkload{
		{PodUID: "a", Identity: "a.ns.mesh.orama.internal", TLS: TLS{Certificate: "a.crt", Key: "a.key", CA: "ca.pem"},
			Routes: []Route{{Destination: "10.0.0.2:5432", Mesh: "10.0.0.2:15008", Identity: "b.ns.mesh.orama.internal"}}},
		{PodUID: "missing", Identity: "c.ns.mesh.orama.internal"},
	}}
	projection := Render(template, func(uid string) (Namespace, bool) {
		return Namespace{Path: "/host/proc/9/ns/net", Inode: 4026533001}, uid == "a"
	})
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"mesh":{"node":"n1","workloads":[{"identity":"a.ns.mesh.orama.internal","network_namespace":"/host/proc/9/ns/net",` +
		`"pod_uid":"a","network_namespace_inode":4026533001,` +
		`"tls":{"certificate":"a.crt","key":"a.key","ca":"ca.pem"},"routes":[{"destination":"10.0.0.2:5432","mesh":"10.0.0.2:15008",` +
		`"identity":"b.ns.mesh.orama.internal"}],"ports":[]}]}}`
	if string(encoded) != want {
		t.Fatalf("projection =\n%s\nwant\n%s", encoded, want)
	}
	empty, _ := json.Marshal(Render(Template{Node: "n2"}, func(string) (Namespace, bool) { return Namespace{}, false }))
	if string(empty) != `{"mesh":{"node":"n2","workloads":[]}}` {
		t.Fatalf("an empty node = %s", empty)
	}
}

func TestWritePublishesFilesBeforeTheProjectionAndRemovesStaleOnes(t *testing.T) {
	dir := t.TempDir()
	first := Projection{Mesh: Mesh{Node: "n1", Workloads: []Workload{{Identity: "a"}}}}
	changed, err := Write(dir, first, map[string][]byte{"a.crt": []byte("A"), "template.json": []byte("never copied"), "../x": []byte("no")})
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.crt")); string(data) != "A" {
		t.Fatal("certificate not written")
	}
	for _, name := range []string{"template.json", "x"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s was published", name)
		}
	}
	if changed, err := Write(dir, first, map[string][]byte{"a.crt": []byte("A")}); err != nil || changed {
		t.Fatalf("an unchanged write reported %v (%v)", changed, err)
	}
	second := Projection{Mesh: Mesh{Node: "n1", Workloads: []Workload{}}}
	if _, err := Write(dir, second, map[string][]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.crt")); err == nil {
		t.Fatal("a stale certificate remains")
	}
	var read Projection
	data, _ := os.ReadFile(filepath.Join(dir, ProjectionFile))
	if err := json.Unmarshal(data, &read); err != nil || len(read.Mesh.Workloads) != 0 {
		t.Fatalf("projection = %s (%v)", data, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() != ProjectionFile {
			t.Fatalf("left behind %s", entry.Name())
		}
	}
}

func TestReadShardReadsOnlyThisNodesTemplateAndItsFiles(t *testing.T) {
	dir := t.TempDir()
	template := Template{Node: "n1", Workloads: []TemplateWorkload{{PodUID: "a", Identity: "a", TLS: TLS{Certificate: "a.crt", Key: "a.key", CA: "ca.pem"}}}}
	encoded, _ := json.Marshal(template)
	for name, data := range map[string][]byte{"n1.json": encoded, "a.crt": []byte("A"), "a.key": []byte("K"), "ca.pem": []byte("C"), "b.key": []byte("other node")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	read, files, err := ReadShard(dir, "n1")
	if err != nil || len(read.Workloads) != 1 || len(files) != 3 || string(files["a.key"]) != "K" {
		t.Fatalf("shard = %+v %v (%v)", read, files, err)
	}
	if _, _, err := ReadShard(dir, "n2"); err != nil {
		t.Fatalf("a node with no template: %v", err)
	}
	bad, _ := json.Marshal(Template{Node: "n3", Workloads: []TemplateWorkload{{TLS: TLS{Certificate: "../../etc/passwd"}}}})
	_ = os.WriteFile(filepath.Join(dir, "n3.json"), bad, 0o400)
	if _, _, err := ReadShard(dir, "n3"); err == nil {
		t.Fatal("a template naming a path outside the shard was read")
	}
	if Shard("aks-node-0") != Shard("aks-node-0") || Shard("aks-node-0") < 0 || Shard("aks-node-0") >= Shards {
		t.Fatal("shard is not stable or not in range")
	}
}
