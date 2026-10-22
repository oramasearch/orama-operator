/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// Package platformcharts reads platform-charts.yaml, the SHA-256 pins of the chart archives and
// CRD manifests helmfile.yaml installs, and stages those files for helmfile: each is downloaded,
// or found already staged, and kept only when its bytes are the pinned ones (orama-operator#808).
// hack/stagecharts runs it; cmd/platform_sync_test.go reads the same pins to compare helmfile's
// path with every leaf's ClusterHelmRelease.
package platformcharts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"

	"sigs.k8s.io/yaml"

	"github.com/oramasearch/orama-operator/internal/controller"
)

// Pins is platform-charts.yaml.
type Pins struct {
	Charts []Chart `json:"charts"`
}

// Chart is one chart repository's chart at a version, and the digest of its archive.
type Chart struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	// CRDs are manifests the chart ships apart from itself (TiDB's crd.yaml).
	CRDs []CRD `json:"crds,omitempty"`
}

// CRD is a CRD manifest and the digest of its bytes.
type CRD struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Archive is the file name the chart's archive is staged under, which helmfile.yaml names.
func (c Chart) Archive() string { return c.Name + "-" + c.Version + ".tgz" }

// CRDFile is the file name a CRD manifest is staged under, which helmfile.yaml's hook names.
func (c Chart) CRDFile(crd CRD) string { return c.Name + "-" + c.Version + "-" + path.Base(crd.URL) }

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Load reads and checks a pins file: every pin is a sha256 digest and no two files are staged
// under one name.
func Load(file string) (*Pins, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var pins Pins
	if err := yaml.UnmarshalStrict(data, &pins); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	staged := map[string]bool{}
	claim := func(name string) error {
		if staged[name] {
			return fmt.Errorf("%s: two files would be staged as %s", file, name)
		}
		staged[name] = true
		return nil
	}
	for _, chart := range pins.Charts {
		if chart.Name == "" || chart.Repository == "" || chart.Version == "" {
			return nil, fmt.Errorf("%s: a chart needs a name, a repository and a version: %+v", file, chart)
		}
		if !digestPattern.MatchString(chart.SHA256) {
			return nil, fmt.Errorf("%s: %s has no sha256 pin", file, chart.Archive())
		}
		if err := claim(chart.Archive()); err != nil {
			return nil, err
		}
		for _, crd := range chart.CRDs {
			if !digestPattern.MatchString(crd.SHA256) {
				return nil, fmt.Errorf("%s: CRD manifest %s has no sha256 pin", file, crd.URL)
			}
			if err := claim(chart.CRDFile(crd)); err != nil {
				return nil, err
			}
		}
	}
	return &pins, nil
}

// Stage puts every pinned file in dir. A file already there is kept when its bytes are the pinned
// ones; otherwise, unless offline, it is downloaded and written only once its bytes are checked.
// Offline, a file missing or not the pinned one is an error. report is told what was done to
// each.
func Stage(ctx context.Context, pins *Pins, dir string, offline bool, report func(string)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, chart := range pins.Charts {
		err := stage(dir, chart.Archive(), offline, report,
			func(data []byte) error {
				return controller.CheckPinnedChart(chart.Archive(), chart.Version, chart.SHA256, data)
			},
			func() ([]byte, error) {
				return controller.DownloadPinnedChart(chart.Repository, chart.Name, chart.Version, chart.SHA256)
			})
		if err != nil {
			return err
		}
		for _, crd := range chart.CRDs {
			err := stage(dir, chart.CRDFile(crd), offline, report,
				func(data []byte) error { return controller.VerifySHA256(data, crd.SHA256) },
				func() ([]byte, error) { return controller.DownloadPinnedManifest(ctx, crd.URL, crd.SHA256) })
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func stage(dir, name string, offline bool, report func(string), check func([]byte) error, download func() ([]byte, error)) error {
	file := filepath.Join(dir, name)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		checkErr := check(data)
		if checkErr == nil {
			report(name + ": staged, and its bytes are the pinned ones")
			return nil
		}
		if offline {
			return fmt.Errorf("%s: %w", file, checkErr)
		}
		report(name + ": staged, but not the pinned bytes (" + checkErr.Error() + "); downloading it again")
	case !errors.Is(err, fs.ErrNotExist):
		return err
	case offline:
		return fmt.Errorf("%s is not staged, and -offline downloads nothing", file)
	}
	if data, err = download(); err != nil {
		return err
	}
	// Written beside the target and renamed over it, so helmfile never finds a partial file.
	tmp, err := os.CreateTemp(dir, ".staging-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	report(name + ": downloaded, and its bytes are the pinned ones")
	return nil
}
