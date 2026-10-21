/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// Package backup is the operator's in-process backup system (operator#453): one Go interface,
// Mechanism, with one implementation per datastore kind, run by the manager binary started as
// `/manager backup --datastore <kind> --backup <name> ...` inside a Job the Backup controller
// creates. The Job needs no Kubernetes credentials: it reads the datastore's connection and the
// bucket's scoped credentials from environment variables, and reports its outcome as the
// container's termination message, which the controller writes to the Backup's status.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Datastore is the connection the Job was given.
type Datastore struct {
	Host     string
	Port     string
	Username string
	Password string
	Database string
}

// Target is where one backup is written: <bucket>/<Prefix>, where Prefix ends in "/".
type Target struct {
	Bucket Bucket
	Prefix string
}

// URL is the S3 URL of the target, path-style on the bucket's endpoint.
func (t Target) URL() string {
	return t.Bucket.Endpoint + "/" + t.Bucket.Name + "/" + t.Prefix
}

// Result is what a mechanism reports.
type Result struct {
	SizeBytes int64  `json:"sizeBytes"`
	Message   string `json:"message"`
}

// Mechanism takes one consistent backup of one datastore kind into the target, using the
// engine's own snapshot rather than reimplementing it.
type Mechanism interface {
	Backup(ctx context.Context, store Datastore, target Target) (Result, error)
}

// mechanisms maps a datastore kind (BackupDatastoreKind) to its mechanism. A kind not here has no
// mechanism yet: the controllers refuse it in status and never start a Job for it.
var mechanisms = map[string]Mechanism{
	"ClickHouse": ClickHouse{},
}

// Supported reports whether a datastore kind has a mechanism.
func Supported(kind string) bool {
	_, ok := mechanisms[kind]
	return ok
}

// SupportedKinds lists the kinds with a mechanism, sorted.
func SupportedKinds() []string {
	kinds := make([]string, 0, len(mechanisms))
	for kind := range mechanisms {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Environment variables the Backup controller sets on the Job's container.
const (
	EnvDatastoreHost     = "DATASTORE_HOST"
	EnvDatastorePort     = "DATASTORE_PORT"
	EnvDatastoreUsername = "DATASTORE_USERNAME"
	EnvDatastorePassword = "DATASTORE_PASSWORD"
	EnvDatastoreDatabase = "DATASTORE_DATABASE"
	EnvBucketEndpoint    = "BUCKET_ENDPOINT"
	EnvBucketName        = "BUCKET_NAME"
	EnvBucketRegion      = "BUCKET_REGION"
	EnvBucketAccessKey   = "BUCKET_ACCESS_KEY_ID"
	EnvBucketSecretKey   = "BUCKET_SECRET_ACCESS_KEY"
)

// Report is the termination message the Job writes; the controller parses it.
type Report struct {
	Succeeded bool   `json:"succeeded"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Message   string `json:"message"`
}

// ParseReport reads a termination message. ok is false when it is not a Report.
func ParseReport(message string) (Report, bool) {
	var report Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(message)), &report); err != nil || report.Message == "" {
		return Report{}, false
	}
	return report, true
}

// terminationLimit keeps the report under the kubelet's 4096-byte termination message cap.
const terminationLimit = 3500

func (r Report) encode() []byte {
	if len(r.Message) > terminationLimit {
		r.Message = r.Message[:terminationLimit] + "…"
	}
	data, _ := json.Marshal(r)
	return data
}

// Main runs `/manager backup ...` and returns the process exit code. args excludes "backup".
func Main(args []string, getenv func(string) string, terminationLog string, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kind := flags.String("datastore", "", "the datastore kind (BackupDatastoreKind)")
	name := flags.String("backup", "", "the Backup's name, for the log")
	prefix := flags.String("prefix", "", "the prefix in the bucket this backup writes under")
	timeout := flags.Duration("timeout", 6*time.Hour, "how long the backup may take")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	report := run(*kind, *name, *prefix, *timeout, getenv)
	if terminationLog != "" {
		_ = os.WriteFile(terminationLog, report.encode(), 0o644)
	}
	fmt.Fprintln(stderr, report.Message)
	if !report.Succeeded {
		return 1
	}
	return 0
}

func run(kind, name, prefix string, timeout time.Duration, getenv func(string) string) Report {
	mechanism, ok := mechanisms[kind]
	if !ok {
		return Report{Message: fmt.Sprintf("no backup mechanism for datastore kind %q; supported: %s", kind, strings.Join(SupportedKinds(), ", "))}
	}
	if name == "" || prefix == "" {
		return Report{Message: "--backup and --prefix are required"}
	}
	bucket, err := BucketFromSecret(map[string][]byte{
		"endpoint":        []byte(getenv(EnvBucketEndpoint)),
		"bucket":          []byte(getenv(EnvBucketName)),
		"region":          []byte(getenv(EnvBucketRegion)),
		"accessKeyId":     []byte(getenv(EnvBucketAccessKey)),
		"secretAccessKey": []byte(getenv(EnvBucketSecretKey)),
	})
	if err != nil {
		return Report{Message: err.Error()}
	}
	store := Datastore{
		Host:     getenv(EnvDatastoreHost),
		Port:     getenv(EnvDatastorePort),
		Username: getenv(EnvDatastoreUsername),
		Password: getenv(EnvDatastorePassword),
		Database: getenv(EnvDatastoreDatabase),
	}
	if store.Host == "" {
		return Report{Message: "the datastore connection Secret has no host"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := mechanism.Backup(ctx, store, Target{Bucket: bucket, Prefix: strings.Trim(prefix, "/") + "/"})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Report{Message: fmt.Sprintf("backup %s did not finish within %s: %v", name, timeout, err)}
		}
		return Report{Message: err.Error()}
	}
	message := result.Message
	if message == "" {
		message = "backup completed"
	}
	return Report{Succeeded: true, SizeBytes: result.SizeBytes, Message: message}
}
