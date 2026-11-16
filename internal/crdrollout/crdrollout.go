// SPDX-License-Identifier: MIT

// Package crdrollout makes a CRD change safe for the operator that is running while it is applied.
//
// The API server prunes every field a CRD's schema does not declare, both on write and when it
// reads a stored object. A CRD that drops a field therefore changes what the running operator sees
// the moment it is applied, before the operator that no longer reads the field has started
// (operator#543: a Lake's pod restarted twice when the Lake feature switches were removed).
//
// Both install paths (build-operator.sh on the managing cluster, the OramaOperator controller on a
// leaf) roll the operator in two CRD steps, which this package computes:
//
//  1. Before the new operator starts, apply Union(chart, cluster): the new CRD, widened by every
//     property the cluster's CRD still has. It only ever adds, so the old operator loses nothing
//     and the new operator already has every field it reads.
//  2. Once the new operator has replaced the old one, apply the chart's CRD exactly. Only now are
//     the dropped fields pruned, and nothing that reads them is running.
//
// Changes renders the difference between two CRDs as field paths, for the test that flags a field
// the previous release reads disappearing from the chart.
package crdrollout

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// normalize makes a CRD comparable whatever decoded it: YAML and the API server give numbers as
// different Go types, which a JSON round trip settles.
func normalize(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	data, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("crdrollout: a CRD that cannot be marshalled: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("crdrollout: a CRD that cannot be unmarshalled: %v", err))
	}
	return out
}

func versions(crd map[string]any) []map[string]any {
	spec, _ := crd["spec"].(map[string]any)
	list, _ := spec["versions"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func schemaOf(version map[string]any) map[string]any {
	schema, _ := version["schema"].(map[string]any)
	root, _ := schema["openAPIV3Schema"].(map[string]any)
	return root
}

// Union returns the chart's CRD widened by everything the cluster's CRD declares that the chart's
// does not: in each version both have, every property only the cluster's schema has (at any
// depth), every enum value only it allows, and a field required only by the chart becomes
// optional. A version only the cluster has is kept, served as it was and never the storage version.
// Everything else is the chart's. cluster may be nil (the CRD is new), and then the result is the
// chart's CRD.
func Union(chart, cluster map[string]any) map[string]any {
	out := normalize(chart)
	if cluster == nil {
		return out
	}
	cluster = normalize(cluster)
	clusterVersions := map[string]map[string]any{}
	for _, v := range versions(cluster) {
		name, _ := v["name"].(string)
		clusterVersions[name] = v
	}
	seen := map[string]bool{}
	merged := []any{}
	for _, v := range versions(out) {
		name, _ := v["name"].(string)
		seen[name] = true
		if old, ok := clusterVersions[name]; ok {
			if newSchema, oldSchema := schemaOf(v), schemaOf(old); newSchema != nil && oldSchema != nil {
				widen(newSchema, oldSchema)
			}
		}
		merged = append(merged, v)
	}
	for _, v := range versions(cluster) {
		name, _ := v["name"].(string)
		if seen[name] {
			continue
		}
		kept := map[string]any{}
		for _, key := range []string{"name", "served", "schema", "subresources", "additionalPrinterColumns", "deprecated", "deprecationWarning", "selectableFields"} {
			if value, ok := v[key]; ok {
				kept[key] = value
			}
		}
		kept["storage"] = false
		merged = append(merged, kept)
	}
	spec, _ := out["spec"].(map[string]any)
	if spec != nil {
		spec["versions"] = merged
	}
	return out
}

// widen adds to next, in place, what old declares and next does not.
func widen(next, old map[string]any) {
	if oldProps, ok := old["properties"].(map[string]any); ok {
		nextProps, _ := next["properties"].(map[string]any)
		if nextProps == nil {
			nextProps = map[string]any{}
			next["properties"] = nextProps
		}
		for name, oldProp := range oldProps {
			nextProp, present := nextProps[name]
			if !present {
				nextProps[name] = oldProp
				continue
			}
			if n, ok := nextProp.(map[string]any); ok {
				if o, ok := oldProp.(map[string]any); ok {
					widen(n, o)
				}
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if n, ok := next[key].(map[string]any); ok {
			if o, ok := old[key].(map[string]any); ok {
				widen(n, o)
			}
		}
	}
	if nextRequired, ok := next["required"].([]any); ok {
		oldRequired, _ := old["required"].([]any)
		both := map[string]bool{}
		for _, r := range oldRequired {
			if s, ok := r.(string); ok {
				both[s] = true
			}
		}
		kept := []any{}
		for _, r := range nextRequired {
			if s, ok := r.(string); ok && both[s] {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(next, "required")
		} else {
			next["required"] = kept
		}
	}
	if nextEnum, ok := next["enum"].([]any); ok {
		if oldEnum, ok := old["enum"].([]any); ok {
			for _, value := range oldEnum {
				found := false
				for _, have := range nextEnum {
					if reflect.DeepEqual(have, value) {
						found = true
						break
					}
				}
				if !found {
					nextEnum = append(nextEnum, value)
				}
			}
			next["enum"] = nextEnum
		}
	}
	if keep, _ := old["x-kubernetes-preserve-unknown-fields"].(bool); keep {
		next["x-kubernetes-preserve-unknown-fields"] = true
	}
}

// Wider reports whether the cluster's CRD declares anything the chart's does not, that is whether
// the exact chart CRD still has to be applied after a rollout to prune what the old operator read.
func Wider(chart, cluster map[string]any) bool {
	if cluster == nil {
		return false
	}
	chart = normalize(chart)
	union := Union(chart, cluster)
	return !reflect.DeepEqual(versions(union), versions(chart))
}

// Change is one difference between two CRDs that is unsafe for an operator reading the older one.
type Change struct {
	CRD     string // the CRD's name, e.g. lakes.orama.cloud
	Version string // the API version, e.g. v1alpha1
	Path    string // the field, e.g. spec.apps.enabled; [] is an array item, {} a map value
	What    string // "removed", "type changed from string to object", "version removed"
}

// Key identifies a change in an allow list: "<crd> <version> <path> <what>", empty parts left out.
func (c Change) Key() string {
	var parts []string
	for _, part := range []string{c.CRD, c.Version, c.Path, c.What} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

// Changes lists what old declares that next no longer does: removed fields, fields whose type
// changed, removed versions. Additions are not listed; they are always safe.
func Changes(old, next map[string]any) []Change {
	old, next = normalize(old), normalize(next)
	meta, _ := next["metadata"].(map[string]any)
	if meta == nil {
		meta, _ = old["metadata"].(map[string]any)
	}
	name, _ := meta["name"].(string)
	nextVersions := map[string]map[string]any{}
	for _, v := range versions(next) {
		n, _ := v["name"].(string)
		nextVersions[n] = v
	}
	var out []Change
	for _, v := range versions(old) {
		version, _ := v["name"].(string)
		nv, ok := nextVersions[version]
		if !ok {
			out = append(out, Change{CRD: name, Version: version, Path: "", What: "version removed"})
			continue
		}
		if o, n := schemaOf(v), schemaOf(nv); o != nil && n != nil {
			compare(name, version, "", o, n, &out)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func compare(crd, version, path string, old, next map[string]any, out *[]Change) {
	oldType, _ := old["type"].(string)
	nextType, _ := next["type"].(string)
	if oldType != "" && nextType != "" && oldType != nextType {
		*out = append(*out, Change{CRD: crd, Version: version, Path: path, What: "type changed from " + oldType + " to " + nextType})
		return
	}
	oldProps, _ := old["properties"].(map[string]any)
	nextProps, _ := next["properties"].(map[string]any)
	names := make([]string, 0, len(oldProps))
	for name := range oldProps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := join(path, name)
		np, present := nextProps[name]
		if !present {
			// A field still accepted by a map that keeps unknown fields is not pruned.
			if keep, _ := next["x-kubernetes-preserve-unknown-fields"].(bool); keep {
				continue
			}
			*out = append(*out, Change{CRD: crd, Version: version, Path: child, What: "removed"})
			continue
		}
		o, _ := oldProps[name].(map[string]any)
		n, _ := np.(map[string]any)
		if o != nil && n != nil {
			compare(crd, version, child, o, n, out)
		}
	}
	if o, ok := old["items"].(map[string]any); ok {
		if n, ok := next["items"].(map[string]any); ok {
			compare(crd, version, path+"[]", o, n, out)
		}
	}
	if o, ok := old["additionalProperties"].(map[string]any); ok {
		if n, ok := next["additionalProperties"].(map[string]any); ok {
			compare(crd, version, path+"{}", o, n, out)
		}
	}
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// Name returns a CRD's metadata.name.
func Name(crd map[string]any) string {
	meta, _ := crd["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

// Parse reads the CustomResourceDefinitions in a multi-document YAML file. Empty documents are
// skipped; any other kind is an error, because a CRD file holds only CRDs.
func Parse(source string, data []byte) ([]map[string]any, error) {
	var out []map[string]any
	for _, doc := range strings.Split("\n"+string(data), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			return nil, fmt.Errorf("%s is not valid YAML: %w", source, err)
		}
		if len(obj) == 0 {
			continue
		}
		if kind, _ := obj["kind"].(string); kind != "CustomResourceDefinition" {
			return nil, fmt.Errorf("%s holds a %q, not a CustomResourceDefinition", source, kind)
		}
		out = append(out, obj)
	}
	return out, nil
}
