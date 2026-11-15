// SPDX-License-Identifier: MIT

// pushchart publishes the operator's chart for one operator version, so a management cluster can
// install that version on a leaf (docs/infrastructure/README.md). The chart goes to its own
// private OCI repository, tagged with the chart version that corresponds to the image tag:
//
//	go run ./hack/pushchart -version dev-83242f9
//	go run ./hack/pushchart -print-version v0.1.0     # prints 0.1.0
//
// Registry credentials are read from $DOCKER_CONFIG/config.json (or ~/.docker/config.json). The
// repository must already exist and be private: Docker Hub creates a repository public on first
// push.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/registry"

	"github.com/oramasearch/orama-operator/internal/controller"
)

func main() {
	version := flag.String("version", "", "the operator version: the image tag this chart installs")
	printVersion := flag.String("print-version", "", "print the chart version for an operator version and exit")
	chartDir := flag.String("chart", "chart", "the chart directory")
	repository := flag.String("repository", "registry-1.docker.io/oramasearch/orama-operator-chart", "the OCI repository to push to")
	flag.Parse()

	if *printVersion != "" {
		fmt.Println(controller.LeafChartVersion(*printVersion))
		return
	}
	if *version == "" {
		fail("-version is required")
	}
	if err := push(*chartDir, *version, *repository); err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "pushchart: "+message)
	os.Exit(1)
}

func push(chartDir, version, repository string) error {
	chrt, err := loader.LoadDir(chartDir)
	if err != nil {
		return fmt.Errorf("load %s: %w", chartDir, err)
	}
	chartVersion := controller.LeafChartVersion(version)
	chrt.Metadata.Version, chrt.Metadata.AppVersion = chartVersion, version

	work, err := os.MkdirTemp("", "pushchart-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	archive, err := chartutil.Save(chrt, work)
	if err != nil {
		return fmt.Errorf("package: %w", err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		return err
	}

	host := strings.SplitN(repository, "/", 2)[0]
	user, pass, err := dockerCredentials(host)
	if err != nil {
		return err
	}
	rc, err := registry.NewClient(registry.ClientOptWriter(io.Discard),
		registry.ClientOptCredentialsFile(filepath.Join(work, "registry.json")))
	if err != nil {
		return err
	}
	if err := rc.Login(host, registry.LoginOptBasicAuth(user, pass)); err != nil {
		return fmt.Errorf("login to %s: %w", host, err)
	}
	ref := repository + ":" + chartVersion
	// Not strict: the repository is named for what it holds (orama-operator-chart), and the
	// chart keeps the name its labels and selectors are built from (orama-operator).
	result, err := rc.Push(data, ref, registry.PushOptStrictMode(false))
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	fmt.Printf("pushed %s (operator %s) %s\n", ref, version, result.Manifest.Digest)
	return nil
}

func dockerCredentials(host string) (string, string, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(home, ".docker")
	}
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("registry credentials: %w", err)
	}
	var config struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	for key, entry := range config.Auths {
		if strings.Contains(key, host) || (strings.HasSuffix(host, "docker.io") && strings.Contains(key, "docker.io")) {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if user, pass, ok := strings.Cut(string(decoded), ":"); err == nil && ok {
				return user, pass, nil
			}
		}
	}
	return "", "", fmt.Errorf("%s has no credentials for %s", path, host)
}
