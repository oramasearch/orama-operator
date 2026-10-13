/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// engineStatefulSets are the writer and reader StatefulSets of a reconciled stack.
func engineStatefulSets(t *testing.T, oc *oramav1alpha1.OramaCloudV1) map[string]*appsv1.StatefulSet {
	t.Helper()
	cl, _ := reconcileOCV1(t, oc)
	return map[string]*appsv1.StatefulSet{
		"writer": getObj(t, cl, "v1-writer", &appsv1.StatefulSet{}),
		"reader": getObj(t, cl, "v1-reader", &appsv1.StatefulSet{}),
	}
}

// startupProbeOf returns the OramaCore container's startup probe.
func startupProbeOf(t *testing.T, sts *appsv1.StatefulSet) *corev1.Probe {
	t.Helper()
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == "oramacore" {
			return c.StartupProbe
		}
	}
	t.Fatalf("%s has no oramacore container", sts.Name)
	return nil
}

// probeWindow is how long a startup probe lets the container take before it is restarted.
func probeWindow(p *corev1.Probe) time.Duration {
	return time.Duration(p.PeriodSeconds) * time.Duration(p.FailureThreshold) * time.Second
}

// preflightOf returns the preflight init container and whether the pod has one.
func preflightOf(sts *appsv1.StatefulSet) (corev1.Container, bool) {
	inits := sts.Spec.Template.Spec.InitContainers
	if i := slices.IndexFunc(inits, func(c corev1.Container) bool { return c.Name == "preflight" }); i >= 0 {
		return inits[i], true
	}
	return corev1.Container{}, false
}

func TestEngineStartupProbeDefaultsFitLargeReaders(t *testing.T) {
	for side, sts := range engineStatefulSets(t, fullStackOCV1(nil)) {
		want := map[string]time.Duration{"writer": 20 * time.Minute, "reader": 40 * time.Minute}[side]
		if got := probeWindow(startupProbeOf(t, sts)); got < want {
			t.Errorf("%s startup probe allows %s, want at least %s", side, got, want)
		}
	}
}

func TestEngineStartupTimeoutSetsFailureThreshold(t *testing.T) {
	sts := engineStatefulSets(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.StartupTimeout = &metav1.Duration{Duration: time.Hour}
		o.Spec.Components.Engine.Writer.StartupTimeout = &metav1.Duration{Duration: 95 * time.Second}
	}))
	if p := startupProbeOf(t, sts["reader"]); p.PeriodSeconds != 10 || p.FailureThreshold != 360 {
		t.Errorf("reader startup probe = %d × %ds, want 360 × 10s", p.FailureThreshold, p.PeriodSeconds)
	}
	// A timeout that is not a whole number of periods rounds up, never shortening it.
	if p := startupProbeOf(t, sts["writer"]); p.FailureThreshold != 10 {
		t.Errorf("writer startup probe failureThreshold = %d, want 10 for 95s", p.FailureThreshold)
	}
}

func TestEngineStartupTimeoutChangeUpdatesPodTemplate(t *testing.T) {
	cl, r := reconcileOCV1(t, fullStackOCV1(nil))
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Reader.StartupTimeout = &metav1.Duration{Duration: time.Hour}
		o.Spec.Components.Engine.Writer.StartupTimeout = &metav1.Duration{Duration: 30 * time.Minute}
	})
	for name, want := range map[string]int32{"v1-reader": 360, "v1-writer": 180} {
		if got := startupProbeOf(t, getObj(t, cl, name, &appsv1.StatefulSet{})).FailureThreshold; got != want {
			t.Errorf("%s startup failureThreshold after the change = %d, want %d", name, got, want)
		}
	}
}

func TestEnginePreflightRunsBeforeRenderConfig(t *testing.T) {
	for _, tc := range []struct {
		name               string
		maps, files        *int64
		wantMaps, wantNOFL string
	}{
		{name: "defaults", wantMaps: "4194304", wantNOFL: "65535"},
		{name: "from the spec", maps: int64Ptr(2097152), files: int64Ptr(131072), wantMaps: "2097152", wantNOFL: "131072"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for side, sts := range engineStatefulSets(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.MinMaxMapCount, o.Spec.Components.Engine.MinOpenFiles = tc.maps, tc.files
			})) {
				var names []string
				for _, c := range sts.Spec.Template.Spec.InitContainers {
					names = append(names, c.Name)
				}
				if want := []string{"wait-for-broker", "preflight", "render-config"}; !slices.Equal(names, want) {
					t.Fatalf("%s init containers = %v, want %v", side, names, want)
				}
				pre, _ := preflightOf(sts)
				cmd := strings.Join(pre.Command, " ")
				for _, want := range []string{"/proc/sys/vm/max_map_count", "-lt " + tc.wantMaps + " ", "ulimit -n", "-lt " + tc.wantNOFL + " "} {
					if !strings.Contains(cmd, want) {
						t.Errorf("%s preflight command lacks %q:\n%s", side, want, cmd)
					}
				}
				sc := pre.SecurityContext
				if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 ||
					sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
					sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
					sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 ||
					sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Errorf("%s preflight security context is not unprivileged and hardened: %+v", side, sc)
				}
			}
		})
	}
}

func TestEnginePreflightZeroSkipsCheck(t *testing.T) {
	for _, tc := range []struct {
		name          string
		maps, files   int64
		present       bool
		with, without string
	}{
		{name: "minMaxMapCount 0", maps: 0, files: 65535, present: true, with: "ulimit -n", without: "max_map_count"},
		{name: "minOpenFiles 0", maps: 4194304, files: 0, present: true, with: "max_map_count", without: "ulimit"},
		{name: "both 0", maps: 0, files: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for side, sts := range engineStatefulSets(t, fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
				o.Spec.Components.Engine.MinMaxMapCount, o.Spec.Components.Engine.MinOpenFiles = &tc.maps, &tc.files
			})) {
				pre, ok := preflightOf(sts)
				if ok != tc.present {
					t.Fatalf("%s has preflight = %v, want %v", side, ok, tc.present)
				}
				if !ok {
					continue
				}
				cmd := strings.Join(pre.Command, " ")
				if !strings.Contains(cmd, tc.with) || strings.Contains(cmd, tc.without) {
					t.Errorf("%s preflight command should check %q and not %q:\n%s", side, tc.with, tc.without, cmd)
				}
			}
		})
	}
}

// TestEnginePreflightScriptFailsBelowMinimum runs the preflight script with a stand-in for
// /proc/sys/vm/max_map_count, so the check's pass, failure and message are exercised.
func TestEnginePreflightScriptFailsBelowMinimum(t *testing.T) {
	pre, ok := preflightInit(fullStackOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.MinOpenFiles = int64Ptr(0) // the host's own limit is not under test
	}))
	if !ok {
		t.Fatal("no preflight container")
	}
	mapCount := filepath.Join(t.TempDir(), "max_map_count")
	script := strings.ReplaceAll(pre.Command[2], "/proc/sys/vm/max_map_count", mapCount)
	for _, tc := range []struct {
		value   string
		wantErr bool
	}{{"1048576", true}, {"4194304", false}} {
		if err := os.WriteFile(mapCount, []byte(tc.value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(pre.Command[0], pre.Command[1], script).CombinedOutput()
		if (err != nil) != tc.wantErr {
			t.Fatalf("max_map_count %s: err = %v, want failure %v\n%s", tc.value, err, tc.wantErr, out)
		}
		if tc.wantErr && !strings.Contains(string(out), "vm.max_map_count is "+tc.value+", below the required 4194304") {
			t.Errorf("failure message does not name the setting:\n%s", out)
		}
	}
}
