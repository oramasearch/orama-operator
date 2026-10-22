/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// What the operator installs on a managed cluster is named by the SHA-256 of its bytes, not only
// by a version (orama-operator#808): a chart archive, a CRD manifest and the operator image. A
// version is what a publisher says an artifact is, and a compromised publisher can move it; a pin
// names the bytes. helmfile's path verifies the same pins through hack/stagecharts.

var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// requirePin refuses a missing or malformed digest. what names the field, for the message.
func requirePin(what, digest string) error {
	if !sha256DigestPattern.MatchString(digest) {
		if digest == "" {
			return fmt.Errorf("%s is required: the artifact is installed only by the SHA-256 of its bytes (sha256:<64 hex digits>)", what)
		}
		return fmt.Errorf("%s %q is not sha256:<64 lowercase hex digits>", what, digest)
	}
	return nil
}

// requireReleasePins refuses a release whose chart, or any of whose CRD manifests, has no pin.
func requireReleasePins(req helmReleaseRequest) error {
	if err := requirePin("the SHA-256 of chart "+req.Chart.String(), req.Chart.SHA256); err != nil {
		return err
	}
	for _, url := range req.CRDURLs {
		if err := requirePin("the SHA-256 of CRD manifest "+url, req.CRDSHA256[url]); err != nil {
			return err
		}
	}
	return nil
}

// VerifySHA256 checks the complete bytes of an artifact against its pin, before any of it is
// read.
func VerifySHA256(data []byte, digest string) error {
	if err := requirePin("the digest", digest); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("its SHA-256 is %s, and the pin is %s: it is not the artifact that was pinned", got, digest)
	}
	return nil
}

// pinnedImageTag is the image tag the operator's chart is given: the version and the digest
// (v0.7.3@sha256:...). The kubelet pulls by the digest, and the version stays readable. It is one
// value so that charts published before the digest existed, which render repository:tag, pin
// the image too.
func pinnedImageTag(version, digest string) string { return version + "@" + digest }

// The Helm release label recording the chart archive a release was installed from, so a pin
// changed at the same version is installed. Helm writes release labels onto its storage Secret,
// whose label values hold at most 63 characters: the digest's 32 bytes in base32 take 52.
const chartPinLabel = "orama.cloud/chart-sha256"

func chartPinLabelValue(digest string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil {
		return ""
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}

// The most a CRD manifest may hold. TiDB's v1.6.6 crd.yaml is 2.8 MiB.
const maxCRDManifestBytes = 64 << 20

// DownloadPinnedManifest downloads a CRD manifest and returns its bytes only when they are the
// pinned ones.
func DownloadPinnedManifest(ctx context.Context, url, digest string) ([]byte, error) {
	if err := requirePin("the digest of CRD manifest "+url, digest); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("CRD manifest %s: %w", url, err)
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("CRD manifest %s cannot be downloaded: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CRD manifest %s cannot be downloaded: HTTP %d", url, response.StatusCode)
	}
	// One byte over the limit, so a manifest cut short at the limit is not taken for a whole one.
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCRDManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("CRD manifest %s cannot be read: %w", url, err)
	}
	if len(data) > maxCRDManifestBytes {
		return nil, fmt.Errorf("CRD manifest %s is larger than %d bytes", url, maxCRDManifestBytes)
	}
	if err := VerifySHA256(data, digest); err != nil {
		return nil, fmt.Errorf("CRD manifest %s: %w", url, err)
	}
	return data, nil
}

// DownloadPinnedChart downloads a chart archive from a chart repository (https://) and returns its
// bytes only when they are the pinned ones and the chart in them is the version asked for.
// hack/stagecharts uses it for helmfile's path, so both installation paths check the same thing.
func DownloadPinnedChart(repository, name, version, digest string) ([]byte, error) {
	source := chartSource{Repository: repository, Name: name, Version: version, SHA256: digest}
	data, err := downloadChartArchive(source)
	if err != nil {
		return nil, err
	}
	if _, err := loadPinnedChart(source.String(), version, digest, data); err != nil {
		return nil, err
	}
	return data, nil
}

// CheckPinnedChart checks archive bytes already on disk as DownloadPinnedChart checks a download.
// what names the archive, for the message.
func CheckPinnedChart(what, version, digest string, data []byte) error {
	_, err := loadPinnedChart(what, version, digest, data)
	return err
}
