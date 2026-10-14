/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	Identity         string  `json:"identity"`
	NetworkNamespace string  `json:"network_namespace"`
	TLS              TLS     `json:"tls"`
	Routes           []Route `json:"routes"`
	Ports            []Port  `json:"ports"`
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

// Resolver finds a pod's network namespace path on this node; ok is false when the pod has no
// process here (not started yet, or gone).
type Resolver func(podUID string) (path string, ok bool)

// Render turns a template into the agent's projection. A pod the resolver cannot find is left out:
// it has no capture yet, and Cilium already refuses its application ports.
func Render(template Template, resolve Resolver) Projection {
	projection := Projection{Mesh: Mesh{Node: template.Node, Workloads: []Workload{}}}
	for _, item := range template.Workloads {
		path, ok := resolve(item.PodUID)
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
			Identity: item.Identity, NetworkNamespace: path, TLS: item.TLS, Routes: routes, Ports: ports,
		})
	}
	return projection
}

// ProcResolver finds a pod's network namespace under a host /proc mount by the pod UID in a
// process's cgroup path (cgroupfs writes the UID as is, the systemd driver with underscores). The
// lowest matching PID, normally the pod sandbox's pause process, names the namespace.
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
	return func(podUID string) (string, bool) {
		if podUID == "" {
			return "", false
		}
		underscored := strings.ReplaceAll(podUID, "-", "_")
		for _, candidate := range processes {
			if strings.Contains(candidate.cgroup, "pod"+podUID) || strings.Contains(candidate.cgroup, "pod"+underscored) {
				return filepath.Join(procRoot, strconv.Itoa(candidate.pid), "ns", "net"), true
			}
		}
		return "", false
	}, nil
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

// Shards is how many Secrets carry the node templates. A node's template is in shard Shard(node),
// as key <node>.json, with the files it names beside it. The DaemonSet mounts every shard, so the
// node projector needs no credential and no API access.
const Shards = 16

// ShardSecret is the name of shard i's Secret.
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
