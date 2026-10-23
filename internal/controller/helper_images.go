/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// Multi-platform manifest digests read from the upstream registries on 2026-10-22.
// Update the tag and digest together after reviewing a new helper release.
const (
	pinnedPostgres16Image = "postgres:16-alpine@sha256:deb28ee471183cb0c303113dac9b2433c51eeeeb678def7af9a9ec5820367815"
	pinnedPostgres18Image = "postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5"
	pinnedCurlImage       = "curlimages/curl:8.10.1@sha256:d9b4541e214bcd85196d6e92e2753ac6d0ea699f0af5741f8c6cccbfcf00ef4b"
)
