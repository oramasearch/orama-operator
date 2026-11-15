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

// Result is what a mechanism reports. Summary is the content fingerprint the backup is verified
// against after a restore (operator#741): what the mechanism counts, per table, database or file
// (Manifest).
type Result struct {
	SizeBytes int64            `json:"sizeBytes"`
	Message   string           `json:"message"`
	Summary   map[string]Count `json:"summary,omitempty"`
}

// Mechanism takes one consistent backup of one datastore kind into the target, using the
// engine's own snapshot rather than reimplementing it, and restores one into an empty datastore of
// the same kind. Restore returns what it observed in the restored datastore (Observed), which is
// compared with the backup's Manifest.
type Mechanism interface {
	Backup(ctx context.Context, store Datastore, target Target) (Result, error)
	Restore(ctx context.Context, store Datastore, source Target, manifest Manifest) (Restored, error)
}

// Cleaner is a mechanism that can empty a scratch datastore it restored into, for a restore
// rehearsal (RestoreRehearsal deletes what it restored, so the next rehearsal finds the target
// empty). It removes only what the manifest says was restored.
type Cleaner interface {
	Cleanup(ctx context.Context, store Datastore, manifest Manifest) error
}

// mechanisms maps a datastore kind (BackupDatastoreKind) to the mechanism the backup Job runs. A
// kind whose own operator takes the backup is in resourceKinds instead; any other kind has no
// mechanism yet and the controllers refuse it in status without starting anything.
var mechanisms = map[string]Mechanism{
	"ClickHouse": ClickHouse{},
}

// resourceKinds are the kinds the controllers back up by writing their own operator's resources
// (a CloudNativePG Backup, a MedusaBackupJob, a TiDB Operator Backup) instead of running a Job.
var resourceKinds = map[string]bool{
	// A CloudNativePG Backup through the Barman Cloud Plugin, which archives WAL continuously
	// (operator#731).
	"Postgres": true,
}

// RunsInJob reports whether a kind's backup and restore run as a Job of the operator's image.
func RunsInJob(kind string) bool {
	_, ok := mechanisms[kind]
	return ok
}

// Supported reports whether a datastore kind has a mechanism, of either sort.
func Supported(kind string) bool {
	return RunsInJob(kind) || resourceKinds[kind]
}

// SupportedKinds lists the kinds with a mechanism, sorted.
func SupportedKinds() []string {
	kinds := make([]string, 0, len(mechanisms)+len(resourceKinds))
	for kind := range mechanisms {
		kinds = append(kinds, kind)
	}
	for kind := range resourceKinds {
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
	// EnvVolumePath is where a volume-backed kind's data is mounted in the Job (operator#738).
	EnvVolumePath = "VOLUME_PATH"
)

// Report is the termination message the Job writes; the controller parses it. Verified and
// Evidence are a restore's: whether the restored content matched the backup's manifest, and what
// was compared.
type Report struct {
	Succeeded bool   `json:"succeeded"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Message   string `json:"message"`
	Verified  bool   `json:"verified,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
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
	if len(r.Evidence) > terminationLimit/3 {
		r.Evidence = r.Evidence[:terminationLimit/3] + "…"
	}
	if len(r.Message)+len(r.Evidence) > terminationLimit {
		r.Message = r.Message[:max(0, terminationLimit-len(r.Evidence))] + "…"
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
		return Report{Message: fmt.Sprintf("no backup mechanism for datastore kind %q runs in a Job; supported: %s", kind, strings.Join(SupportedKinds(), ", "))}
	}
	if name == "" || prefix == "" {
		return Report{Message: "--backup and --prefix are required"}
	}
	bucket, store, err := fromEnvironment(getenv)
	if err != nil {
		return Report{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	target := Target{Bucket: bucket, Prefix: strings.Trim(prefix, "/") + "/"}
	result, err := mechanism.Backup(ctx, store, target)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Report{Message: fmt.Sprintf("backup %s did not finish within %s: %v", name, timeout, err)}
		}
		return Report{Message: redact(err, bucket.SecretAccessKey).Error()}
	}
	// The manifest is what a restore reads and verifies against; without it the backup is not one.
	manifest := Manifest{Kind: kind, Backup: name, CompletedAt: time.Now().UTC(), SizeBytes: result.SizeBytes, Summary: result.Summary}
	if err := writeManifest(ctx, target, manifest); err != nil {
		return Report{Message: fmt.Sprintf("backup %s was written but its manifest was not: %v", name, redact(err, bucket.SecretAccessKey))}
	}
	message := result.Message
	if message == "" {
		message = "backup completed"
	}
	return Report{Succeeded: true, SizeBytes: result.SizeBytes, Message: message}
}

// fromEnvironment reads the bucket and the datastore connection the controller gave the Job.
func fromEnvironment(getenv func(string) string) (Bucket, Datastore, error) {
	bucket, err := BucketFromSecret(map[string][]byte{
		"endpoint":        []byte(getenv(EnvBucketEndpoint)),
		"bucket":          []byte(getenv(EnvBucketName)),
		"region":          []byte(getenv(EnvBucketRegion)),
		"accessKeyId":     []byte(getenv(EnvBucketAccessKey)),
		"secretAccessKey": []byte(getenv(EnvBucketSecretKey)),
	})
	if err != nil {
		return Bucket{}, Datastore{}, err
	}
	store := Datastore{
		Host:     getenv(EnvDatastoreHost),
		Port:     getenv(EnvDatastorePort),
		Username: getenv(EnvDatastoreUsername),
		Password: getenv(EnvDatastorePassword),
		Database: getenv(EnvDatastoreDatabase),
	}
	if store.Host == "" && getenv(EnvVolumePath) == "" {
		return Bucket{}, Datastore{}, errors.New("the datastore connection Secret has no host")
	}
	return bucket, store, nil
}
