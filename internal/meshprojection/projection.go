// SPDX-License-Identifier: MIT

// Package meshprojection is the contract between the operator, which decides which pods on a node
// are enrolled in zetta-tunnel's mesh and what they may reach, and the node projector, which alone
// can find each pod's network namespace on that node. The operator writes a Template per node; the
// projector renders zetta-tunnel's mesh projection (zetta-tunnel docs/mesh.md) from it.
package meshprojection

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// TemplateKey is the key of the template in a node's Secret; every other key is a file beside it.
const TemplateKey = "template.json"

// ProjectionFile is the file the node agent reads (--configuration).
const ProjectionFile = "mesh.json"

// Template is one node's enrolled workloads, by pod UID instead of network namespace.
type Template struct {
	Node      string             `json:"node"`
	Workloads []TemplateWorkload `json:"workloads"`
}

// TemplateWorkload is one enrolled pod.
type TemplateWorkload struct {
	PodUID   string  `json:"pod_uid"`
	Identity string  `json:"identity"`
	TLS      TLS     `json:"tls"`
	Routes   []Route `json:"routes"`
	Ports    []Port  `json:"ports"`
}

// Projection is zetta-tunnel's mesh projection, exactly.
type Projection struct {
	Mesh Mesh `json:"mesh"`
}

type Mesh struct {
	Node      string     `json:"node"`
	Workloads []Workload `json:"workloads"`
}

type Workload struct {
	Identity         string `json:"identity"`
	NetworkNamespace string `json:"network_namespace"`
	// PodUID is the pod's UID from the Kubernetes API (the template's), and NetworkNamespaceInode
	// the inode of the namespace the projector opened for it. The agent checks both before it
	// enters the namespace and refuses a projection without them (zetta-tunnel LSR-264,
	// orama-operator#869), so a recycled PID cannot redirect a workload's identity.
	PodUID                string  `json:"pod_uid"`
	NetworkNamespaceInode uint64  `json:"network_namespace_inode"`
	TLS                   TLS     `json:"tls"`
	Routes                []Route `json:"routes"`
	Ports                 []Port  `json:"ports"`
}

type TLS struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
	CA          string `json:"ca"`
}

type Route struct {
	Destination string `json:"destination"`
	Mesh        string `json:"mesh"`
	Identity    string `json:"identity"`
}

type Port struct {
	Port    int32    `json:"port"`
	Target  string   `json:"target"`
	Sources []string `json:"sources"`
}

// Namespace is a pod's network namespace on this node: the path the agent opens and the inode the
// projector found there.
type Namespace struct {
	Path  string
	Inode uint64
}

// Resolver finds a pod's network namespace on this node; ok is false when the pod has no process
// here (not started yet, or gone) or its namespace could not be pinned.
type Resolver func(podUID string) (namespace Namespace, ok bool)

// Render turns a template into the agent's projection. A pod the resolver cannot find is left out:
// it has no capture yet, and Cilium already refuses its application ports.
func Render(template Template, resolve Resolver) Projection {
	projection := Projection{Mesh: Mesh{Node: template.Node, Workloads: []Workload{}}}
	for _, item := range template.Workloads {
		namespace, ok := resolve(item.PodUID)
		if !ok {
			continue
		}
		routes := item.Routes
		if routes == nil {
			routes = []Route{}
		}
		ports := item.Ports
		if ports == nil {
			ports = []Port{}
		}
		projection.Mesh.Workloads = append(projection.Mesh.Workloads, Workload{
			Identity: item.Identity, NetworkNamespace: namespace.Path, PodUID: item.PodUID,
			NetworkNamespaceInode: namespace.Inode, TLS: item.TLS, Routes: routes, Ports: ports,
		})
	}
	return projection
}

// ProcResolver finds a pod's network namespace under a host /proc mount. A process belongs to the
// pod when its cgroup names exactly that pod (cgroupfs writes pod<uid>, the systemd driver
// kubepods[-burstable|-besteffort]-pod<uid with underscores>.slice) and names no other pod; a UID
// that is only a prefix is not membership. Of the member processes the lowest PID, normally the pod
// sandbox's pause process, names the namespace.
//
// The namespace is pinned the way the agent checks it (zetta-tunnel docs/mesh.md): the process
// directory is opened once, membership is read through it, ns/net is opened relative to it and its
// inode read with fstat, and membership is read again; a process that disappeared or changed in
// between is skipped. Opening another process's ns/net needs CAP_SYS_PTRACE.
func ProcResolver(procRoot string) (Resolver, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	type process struct {
		pid    int
		cgroup string
	}
	var processes []process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cgroup"))
		if err != nil {
			continue
		}
		processes = append(processes, process{pid: pid, cgroup: string(data)})
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	return func(podUID string) (Namespace, bool) {
		if !podUIDPattern.MatchString(podUID) {
			return Namespace{}, false
		}
		for _, candidate := range processes {
			if !CgroupNamesPod(candidate.cgroup, podUID) {
				continue
			}
			directory := filepath.Join(procRoot, strconv.Itoa(candidate.pid))
			inode, err := pinNetworkNamespace(directory, podUID)
			if err != nil {
				continue
			}
			return Namespace{Path: filepath.Join(directory, "ns", "net"), Inode: inode}, true
		}
		return Namespace{}, false
	}, nil
}

// podUIDPattern is a lowercase UUID, the form the Kubernetes API gives a pod UID.
var podUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CgroupNamesPod reports whether a /proc/<pid>/cgroup file places the process in exactly that pod:
// some path component names it, and no path component names another pod.
func CgroupNamesPod(cgroup, podUID string) bool {
	underscored := strings.ReplaceAll(podUID, "-", "_")
	accepted := map[string]bool{
		"pod" + podUID:                                     true,
		"kubepods-pod" + underscored + ".slice":            true,
		"kubepods-burstable-pod" + underscored + ".slice":  true,
		"kubepods-besteffort-pod" + underscored + ".slice": true,
	}
	matched := false
	for _, line := range strings.Split(cgroup, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "/") {
			return false
		}
		for _, component := range strings.Split(fields[2], "/") {
			if !strings.HasPrefix(component, "pod") && !strings.Contains(component, "-pod") {
				continue
			}
			if !accepted[component] {
				return false
			}
			matched = true
		}
	}
	return matched
}

// pinNetworkNamespace opens a process directory once, checks its pod membership, opens ns/net
// relative to it, reads the namespace's inode, and checks membership again.
func pinNetworkNamespace(directory, podUID string) (uint64, error) {
	dir, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(dir)
	member := func() error {
		data, err := readAt(dir, "cgroup")
		if err != nil {
			return err
		}
		if !CgroupNamesPod(data, podUID) {
			return errors.New("the process is not in the pod")
		}
		return nil
	}
	if err := member(); err != nil {
		return 0, err
	}
	namespace, err := unix.Openat(dir, "ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(namespace)
	var status unix.Stat_t
	if err := unix.Fstat(namespace, &status); err != nil {
		return 0, err
	}
	if err := member(); err != nil {
		return 0, err
	}
	if status.Ino == 0 {
		return 0, errors.New("the namespace has no inode")
	}
	return uint64(status.Ino), nil //nolint:unconvert // Ino is not uint64 on every platform.
}

// cgroupLimit bounds a cgroup file, as the agent does.
const cgroupLimit = 64 * 1024

func readAt(dir int, name string) (string, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cgroupLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > cgroupLimit {
		return "", errors.New("the cgroup file is too large")
	}
	return string(data), nil
}

// Write publishes files and then the projection into dir, each by atomic rename, and removes files
// the projection no longer names. The projection is written last, so the agent never reads one
// that names a file not yet there.
func Write(dir string, projection Projection, files map[string][]byte) (bool, error) {
	encoded, err := json.Marshal(projection)
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	current, err := os.ReadFile(filepath.Join(dir, ProjectionFile))
	changed := err != nil || !bytes.Equal(current, encoded)
	keep := map[string]bool{ProjectionFile: true}
	for name, data := range files {
		if name == TemplateKey || name == ProjectionFile || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
			continue
		}
		keep[name] = true
		existing, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := atomicWrite(dir, name, data, 0o400); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		if err := atomicWrite(dir, ProjectionFile, encoded, 0o444); err != nil {
			return false, err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return changed, err
	}
	for _, entry := range entries {
		if !keep[entry.Name()] && !strings.HasPrefix(entry.Name(), ".") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return changed, err
			}
		}
	}
	return changed, nil
}

func atomicWrite(dir, name string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(temporary.Name()) }
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		cleanup()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(temporary.Name(), filepath.Join(dir, name)); err != nil {
		cleanup()
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// Shards is how many directories a node's template could be read from. Each node's template is in
// that node's own Secret, as key <node>.json with the files it names beside it, and the node's agent
// mounts only that Secret, at <templates>/<Shard(node)> (orama-operator#788). Before #788 the
// operator wrote every node's template into one of Shards shared Secrets and every agent mounted all
// of them, which delivered every node's workload private keys to every node. The node projector
// needs no credential and no API access.
const Shards = 16

// ShardSecret is the name of shard i's Secret before #788; the operator deletes these.
func ShardSecret(i int) string { return "mesh-templates-" + strconv.Itoa(i) }

// Shard is the shard a node's template is in: FNV-1a of the node name.
func Shard(node string) int {
	var hash uint32 = 2166136261
	for i := 0; i < len(node); i++ {
		hash ^= uint32(node[i])
		hash *= 16777619
	}
	return int(hash % Shards)
}

// TemplateFile is the key of a node's template in its shard.
func TemplateFile(node string) string { return node + ".json" }

// ReadShard reads a node's template and the files it names from the mounted shard directory. A
// missing template is an empty one: no pod on this node is enrolled.
func ReadShard(dir, node string) (Template, map[string][]byte, error) {
	template := Template{Node: node}
	data, err := os.ReadFile(filepath.Join(dir, TemplateFile(node)))
	if errors.Is(err, os.ErrNotExist) {
		return template, map[string][]byte{}, nil
	}
	if err != nil {
		return template, nil, err
	}
	if err := json.Unmarshal(data, &template); err != nil {
		return template, nil, err
	}
	if template.Node != node {
		return template, nil, fmt.Errorf("the template is for node %q", template.Node)
	}
	files := map[string][]byte{}
	for _, workload := range template.Workloads {
		for _, name := range []string{workload.TLS.Certificate, workload.TLS.Key, workload.TLS.CA} {
			if _, done := files[name]; done || name == "" {
				continue
			}
			if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
				return template, nil, fmt.Errorf("the template names the file %q", name)
			}
			content, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return template, nil, err
			}
			files[name] = content
		}
	}
	return template, files, nil
}
