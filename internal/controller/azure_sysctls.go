// SPDX-License-Identifier: MIT

package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// These wire types follow SysctlConfig in the AKS 2025-05-01 API. Pointers preserve
// explicit zero/false and distinguish an unreported setting from a reported default.
type azureLinuxOSConfig struct {
	Sysctls *azureSysctlConfig `json:"sysctls,omitempty"`
}

type azureSysctlConfig struct {
	NetCoreSomaxconn               *int32  `json:"netCoreSomaxconn,omitempty"`
	NetCoreNetdevMaxBacklog        *int32  `json:"netCoreNetdevMaxBacklog,omitempty"`
	NetCoreRmemDefault             *int32  `json:"netCoreRmemDefault,omitempty"`
	NetCoreRmemMax                 *int32  `json:"netCoreRmemMax,omitempty"`
	NetCoreWmemDefault             *int32  `json:"netCoreWmemDefault,omitempty"`
	NetCoreWmemMax                 *int32  `json:"netCoreWmemMax,omitempty"`
	NetCoreOptmemMax               *int32  `json:"netCoreOptmemMax,omitempty"`
	NetIpv4TcpMaxSynBacklog        *int32  `json:"netIpv4TcpMaxSynBacklog,omitempty"`
	NetIpv4TcpMaxTwBuckets         *int32  `json:"netIpv4TcpMaxTwBuckets,omitempty"`
	NetIpv4TcpFinTimeout           *int32  `json:"netIpv4TcpFinTimeout,omitempty"`
	NetIpv4TcpKeepaliveTime        *int32  `json:"netIpv4TcpKeepaliveTime,omitempty"`
	NetIpv4TcpKeepaliveProbes      *int32  `json:"netIpv4TcpKeepaliveProbes,omitempty"`
	NetIpv4TcpkeepaliveIntvl       *int32  `json:"netIpv4TcpkeepaliveIntvl,omitempty"`
	NetIpv4TcpTwReuse              *bool   `json:"netIpv4TcpTwReuse,omitempty"`
	NetIpv4IpLocalPortRange        *string `json:"netIpv4IpLocalPortRange,omitempty"`
	NetIpv4NeighDefaultGcThresh1   *int32  `json:"netIpv4NeighDefaultGcThresh1,omitempty"`
	NetIpv4NeighDefaultGcThresh2   *int32  `json:"netIpv4NeighDefaultGcThresh2,omitempty"`
	NetIpv4NeighDefaultGcThresh3   *int32  `json:"netIpv4NeighDefaultGcThresh3,omitempty"`
	NetNetfilterNfConntrackMax     *int32  `json:"netNetfilterNfConntrackMax,omitempty"`
	NetNetfilterNfConntrackBuckets *int32  `json:"netNetfilterNfConntrackBuckets,omitempty"`
	FsInotifyMaxUserWatches        *int32  `json:"fsInotifyMaxUserWatches,omitempty"`
	FsFileMax                      *int32  `json:"fsFileMax,omitempty"`
	FsAioMaxNr                     *int32  `json:"fsAioMaxNr,omitempty"`
	FsNrOpen                       *int32  `json:"fsNrOpen,omitempty"`
	KernelThreadsMax               *int32  `json:"kernelThreadsMax,omitempty"`
	VmMaxMapCount                  *int32  `json:"vmMaxMapCount,omitempty"`
	VmSwappiness                   *int32  `json:"vmSwappiness,omitempty"`
	VmVfsCachePressure             *int32  `json:"vmVfsCachePressure,omitempty"`
}

// aksIntegerSysctl binds a Linux name and its supported interval to a typed wire field.
type aksIntegerSysctl struct {
	field    **int32
	min, max int32
}

// Intervals follow Microsoft's custom-node-configuration-reference. The REST API
// limits integer fields to int32, including fs.file-max despite its wider kernel range.
// Fields with no documented interval accept positive int32 values.
func aksIntegerSysctls(config *azureSysctlConfig) map[string]aksIntegerSysctl {
	return map[string]aksIntegerSysctl{
		"net.core.somaxconn":                 {&config.NetCoreSomaxconn, 4096, 3240000},
		"net.core.netdev_max_backlog":        {&config.NetCoreNetdevMaxBacklog, 1000, 3240000},
		"net.core.rmem_default":              {&config.NetCoreRmemDefault, 1, 2147483647},
		"net.core.rmem_max":                  {&config.NetCoreRmemMax, 212992, 134217728},
		"net.core.wmem_default":              {&config.NetCoreWmemDefault, 1, 2147483647},
		"net.core.wmem_max":                  {&config.NetCoreWmemMax, 212992, 134217728},
		"net.core.optmem_max":                {&config.NetCoreOptmemMax, 20480, 4194304},
		"net.ipv4.tcp_max_syn_backlog":       {&config.NetIpv4TcpMaxSynBacklog, 128, 3240000},
		"net.ipv4.tcp_max_tw_buckets":        {&config.NetIpv4TcpMaxTwBuckets, 8000, 1440000},
		"net.ipv4.tcp_fin_timeout":           {&config.NetIpv4TcpFinTimeout, 5, 120},
		"net.ipv4.tcp_keepalive_time":        {&config.NetIpv4TcpKeepaliveTime, 30, 432000},
		"net.ipv4.tcp_keepalive_probes":      {&config.NetIpv4TcpKeepaliveProbes, 1, 15},
		"net.ipv4.tcp_keepalive_intvl":       {&config.NetIpv4TcpkeepaliveIntvl, 10, 90},
		"net.ipv4.neigh.default.gc_thresh1":  {&config.NetIpv4NeighDefaultGcThresh1, 128, 80000},
		"net.ipv4.neigh.default.gc_thresh2":  {&config.NetIpv4NeighDefaultGcThresh2, 512, 90000},
		"net.ipv4.neigh.default.gc_thresh3":  {&config.NetIpv4NeighDefaultGcThresh3, 1024, 100000},
		"net.netfilter.nf_conntrack_max":     {&config.NetNetfilterNfConntrackMax, 131072, 2097152},
		"net.netfilter.nf_conntrack_buckets": {&config.NetNetfilterNfConntrackBuckets, 65536, 524288},
		"fs.inotify.max_user_watches":        {&config.FsInotifyMaxUserWatches, 781250, 2097152},
		"fs.file-max":                        {&config.FsFileMax, 8192, 2147483647},
		"fs.aio-max-nr":                      {&config.FsAioMaxNr, 65536, 6553500},
		"fs.nr_open":                         {&config.FsNrOpen, 8192, 20000500},
		"kernel.threads-max":                 {&config.KernelThreadsMax, 1, 2147483647},
		"vm.max_map_count":                   {&config.VmMaxMapCount, 1, 2147483647},
		"vm.swappiness":                      {&config.VmSwappiness, 0, 100},
		"vm.vfs_cache_pressure":              {&config.VmVfsCachePressure, 1, 100},
	}
}

// aksLinuxOSConfig validates the provider's supported subset and maps kernel names
// to AKS fields. No configuration is sent for an empty map.
func aksLinuxOSConfig(sysctls oramav1alpha1.NodeSysctls) (*azureLinuxOSConfig, error) {
	if len(sysctls) == 0 {
		return nil, nil
	}
	config := &azureSysctlConfig{}
	integers := aksIntegerSysctls(config)
	for _, name := range sortedSysctlNames(sysctls) {
		value := sysctls[name]
		switch name {
		case "net.ipv4.tcp_tw_reuse":
			if value.Type != intstr.Int || (value.IntVal != 0 && value.IntVal != 1) {
				return nil, fmt.Errorf("sysctl %s: AKS requires integer 0 or 1", name)
			}
			enabled := value.IntVal == 1
			config.NetIpv4TcpTwReuse = &enabled
		case "net.ipv4.ip_local_port_range":
			if value.Type != intstr.String {
				return nil, fmt.Errorf("sysctl %s: AKS requires a string containing two ports", name)
			}
			ports := strings.Fields(value.StrVal)
			if len(ports) != 2 {
				return nil, fmt.Errorf("sysctl %s: AKS requires two space-separated ports", name)
			}
			first, firstErr := strconv.Atoi(ports[0])
			last, lastErr := strconv.Atoi(ports[1])
			if firstErr != nil || lastErr != nil || first < 1024 || first > 60999 || last < 32768 || last > 65535 || first >= last {
				return nil, fmt.Errorf("sysctl %s: AKS requires first port 1024..60999, last port 32768..65535, and first < last", name)
			}
			normalized := fmt.Sprintf("%d %d", first, last)
			config.NetIpv4IpLocalPortRange = &normalized
		default:
			setting, supported := integers[name]
			if !supported {
				return nil, fmt.Errorf("sysctl %s: not supported by AKS API 2025-05-01", name)
			}
			if value.Type != intstr.Int || value.IntVal < setting.min || value.IntVal > setting.max {
				return nil, fmt.Errorf("sysctl %s: AKS requires an integer in %d..%d", name, setting.min, setting.max)
			}
			number := value.IntVal
			*setting.field = &number
		}
	}
	return &azureLinuxOSConfig{Sysctls: config}, nil
}

func sortedSysctlNames(sysctls oramav1alpha1.NodeSysctls) []string {
	names := make([]string, 0, len(sysctls))
	for name := range sysctls {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// aksReportedSysctls reads only fields Azure actually returned. Absence is unknown,
// not a guessed OS default: ARM's pool configuration is not a runtime kernel probe.
func aksReportedSysctls(linuxOS *azureLinuxOSConfig) oramav1alpha1.NodeSysctls {
	values := oramav1alpha1.NodeSysctls{}
	if linuxOS == nil || linuxOS.Sysctls == nil {
		return values
	}
	config := linuxOS.Sysctls
	for name, setting := range aksIntegerSysctls(config) {
		if *setting.field != nil {
			values[name] = intstr.FromInt32(**setting.field)
		}
	}
	if config.NetIpv4TcpTwReuse != nil {
		value := int32(0)
		if *config.NetIpv4TcpTwReuse {
			value = 1
		}
		values["net.ipv4.tcp_tw_reuse"] = intstr.FromInt32(value)
	}
	if config.NetIpv4IpLocalPortRange != nil {
		values["net.ipv4.ip_local_port_range"] = intstr.FromString(strings.Join(strings.Fields(*config.NetIpv4IpLocalPortRange), " "))
	}
	return values
}

func aksSysctlDrift(pool oramav1alpha1.AksNodePool, linuxOS *azureLinuxOSConfig) []string {
	// Desired settings have already been validated before the Azure read.
	desired, err := aksLinuxOSConfig(pool.Sysctls)
	if err != nil {
		return []string{fmt.Sprintf("node pool %s: %s", pool.Name, err)}
	}
	want := aksReportedSysctls(desired)
	have := aksReportedSysctls(linuxOS)
	var drift []string
	for _, name := range sortedSysctlNames(want) {
		actual, reported := have[name]
		expected := want[name]
		if !reported {
			drift = append(drift, fmt.Sprintf("node pool %s: sysctl %s: want %s, Azure does not report a value", pool.Name, name, expected.String()))
		} else if actual != expected {
			drift = append(drift, fmt.Sprintf("node pool %s: sysctl %s: want %s, cluster has %s", pool.Name, name, expected.String(), actual.String()))
		}
	}
	return drift
}
