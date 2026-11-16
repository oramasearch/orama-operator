/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A Lake's regions (operator#512): one Lake per region, each at its own host, and a
// region picker at the apex host that preselects the region closest to the visitor. The operator
// supplies everything the picker needs, so lake has no region setting of its own.

// lakeRegionEnvNames are set only from spec.region, spec.regions and spec.apexHost; the same names
// in spec.env are ignored.
var lakeRegionEnvNames = []string{"LAKE_REGION", "LAKE_REGION_LABEL", "LAKE_REGIONS", "LAKE_APEX_HOST"}

func lakeRegionEnv(lake *oramav1alpha1.Lake) []corev1.EnvVar {
	var env []corev1.EnvVar
	if region := lake.Spec.Region; region != nil {
		if region.Name != "" {
			env = append(env, corev1.EnvVar{Name: "LAKE_REGION", Value: region.Name})
		}
		if region.Label != "" {
			env = append(env, corev1.EnvVar{Name: "LAKE_REGION_LABEL", Value: region.Label})
		}
	}
	if len(lake.Spec.Regions) > 0 {
		raw, _ := json.Marshal(lake.Spec.Regions)
		env = append(env, corev1.EnvVar{Name: "LAKE_REGIONS", Value: string(raw)})
	}
	if lakeApexHost(lake) != "" {
		env = append(env, corev1.EnvVar{Name: "LAKE_APEX_HOST", Value: lake.Spec.ApexHost})
	}
	return env
}

// lakeApexHost is the apex host the Lake answers, or "" when it has none or is not exposed.
func lakeApexHost(lake *oramav1alpha1.Lake) string {
	if !lake.Spec.IngressEnabled || lake.Spec.Host == "" || lake.Spec.ApexHost == lake.Spec.Host {
		return ""
	}
	return lake.Spec.ApexHost
}

// cloudRegionLocations locates each cloud region's datacenter, in decimal degrees, as Azure
// publishes it (`az account list-locations`, metadata.latitude and metadata.longitude). A LakeSet
// member in a region missing here is refused until the region is added.
var cloudRegionLocations = map[string][2]string{
	"canadacentral":      {"43.653", "-79.383"},
	"centralus":          {"41.5908", "-93.6208"},
	"eastus":             {"37.3719", "-79.8164"},
	"eastus2":            {"36.6681", "-78.3889"},
	"francecentral":      {"46.3772", "2.373"},
	"germanywestcentral": {"50.110924", "8.682127"},
	"northeurope":        {"53.3478", "-6.2597"},
	"southcentralus":     {"29.4167", "-98.5"},
	"swedencentral":      {"60.67488", "17.14127"},
	"uksouth":            {"50.941", "-0.799"},
	"westeurope":         {"52.3667", "4.9"},
	"westus":             {"37.783", "-122.417"},
	"westus2":            {"47.233", "-119.852"},
	"westus3":            {"33.448376", "-112.074036"},
}

// prepareLakeRegions is a LakeSet's region list: one entry per member that states a region label,
// each with its cluster's region, its host and the region's location. A member whose cluster does
// not exist yet is left out until it does. It refuses the set when the list cannot be right: a
// labelled member with no host, a region with no known location, a list stated by hand, two
// apex hosts, or one region named twice: a picker chooses between the regions of one environment,
// never between environments in one region (#532).
func prepareLakeRegions(template map[string]any, members []resolvedMember) (any, error) {
	var regions []oramav1alpha1.LakeRegion
	var apexes []string
	byRegion := map[string]string{}
	for _, resolved := range members {
		spec, err := memberSpec(template, resolved)
		if err != nil {
			continue // the member fails on its own when it is applied
		}
		if _, stated := spec["regions"]; stated {
			return nil, &setError{"RegionsStated", fmt.Sprintf("member %s states spec.regions, which the set computes from its members: remove it from the template and the member's spec", resolved.member.Name)}
		}
		if apex, _ := spec["apexHost"].(string); apex != "" {
			apexes = append(apexes, resolved.member.Name+" ("+apex+")")
		}
		region, _ := spec["region"].(map[string]any)
		label, _ := region["label"].(string)
		if label == "" || resolved.cluster == nil {
			continue
		}
		host, _ := spec["host"].(string)
		if host == "" {
			return nil, &setError{"RegionWithoutHost", fmt.Sprintf("member %s has the region label %q but no spec.host, so the region picker has nowhere to send people", resolved.member.Name, label)}
		}
		if other, taken := byRegion[resolved.cluster.Spec.Region]; taken {
			return nil, &setError{"RegionRepeated", fmt.Sprintf("members %s and %s both have a region label and both run in %s: a region picker chooses between the regions of one environment, so each region appears once; remove the region from the members that are separate environments", other, resolved.member.Name, resolved.cluster.Spec.Region)}
		}
		byRegion[resolved.cluster.Spec.Region] = resolved.member.Name
		location, known := cloudRegionLocations[resolved.cluster.Spec.Region]
		if !known {
			return nil, &setError{"UnknownRegion", fmt.Sprintf("member %s runs on %s, in region %q, whose location the operator does not know: add it to cloudRegionLocations", resolved.member.Name, resolved.cluster.Name, resolved.cluster.Spec.Region)}
		}
		regions = append(regions, oramav1alpha1.LakeRegion{Name: resolved.cluster.Spec.Region, Label: label, Host: host, Latitude: location[0], Longitude: location[1]})
	}
	if len(apexes) > 1 {
		sort.Strings(apexes)
		return nil, &setError{"TwoApexHosts", "at most one member serves the apex host, and these all set spec.apexHost: " + strings.Join(apexes, ", ")}
	}
	return regions, nil
}

// finishLakeRegion gives a member that states a region its cluster's region name and the set's
// region list. A member that states no region is left as it is.
func finishLakeRegion(spec map[string]any, cluster *oramav1alpha1.KubernetesCluster, shared any) error {
	region, _ := spec["region"].(map[string]any)
	if region == nil {
		return nil
	}
	if set, ok := region["name"].(string); ok && set != "" && set != cluster.Spec.Region {
		return fmt.Errorf("spec.region.name is %q, but the instance runs on %s, which is in %s: the region comes from the cluster, remove it", set, cluster.Name, cluster.Spec.Region)
	}
	region["name"] = cluster.Spec.Region
	regions, _ := shared.([]oramav1alpha1.LakeRegion)
	if label, _ := region["label"].(string); label == "" || len(regions) == 0 {
		return nil
	}
	raw, err := json.Marshal(regions)
	if err != nil {
		return err
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	spec["regions"] = list
	return nil
}
