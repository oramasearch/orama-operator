/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

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

// Restore and its verification (operator#741). Every backup a Job takes ends with a manifest,
// <prefix>orama-backup.json: the kind, the Backup, when it completed, and a Summary of what the
// mechanism counted. `/manager restore` reads it, restores the backup into a datastore of the same
// kind that holds nothing yet, counts the same things in what it restored, and passes only when every
// count is within what the manifest recorded. "A backup that has never been restored is not counted as
// working" (operator#453): a restore rehearsal is this, on a schedule, into a scratch datastore.

// ManifestKey is the manifest's object name under a backup's prefix.
const ManifestKey = "orama-backup.json"

// Count is what a mechanism counted of one thing (a table's rows, a database's keys, a file's
// bytes). A server that keeps taking writes while it is backed up is counted before and after its
// snapshot, so the restored value must lie between Min and Max; an exact count has Min == Max.
type Count struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Exact is a count that does not move.
func Exact(n int64) Count { return Count{Min: n, Max: n} }

// Manifest is what a backup records about itself.
type Manifest struct {
	Kind        string           `json:"kind"`
	Backup      string           `json:"backup"`
	CompletedAt time.Time        `json:"completedAt"`
	SizeBytes   int64            `json:"sizeBytes"`
	Summary     map[string]Count `json:"summary,omitempty"`
	// Extra is mechanism-specific (for example the files of a volume backup).
	Extra json.RawMessage `json:"extra,omitempty"`
}

// Restored is what a mechanism observed in the datastore it restored into.
type Restored struct {
	Observed map[string]int64
	Message  string
	// Details is what the mechanism's own Cleanup needs from its restore.
	Details map[string]string
}

func writeManifest(ctx context.Context, target Target, manifest Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return (&S3{Bucket: target.Bucket}).Put(ctx, target.Prefix+ManifestKey, data)
}

// ReadManifest reads a backup's manifest.
func ReadManifest(ctx context.Context, source Target) (Manifest, error) {
	data, err := (&S3{Bucket: source.Bucket}).Get(ctx, source.Prefix+ManifestKey)
	if err != nil {
		return Manifest{}, fmt.Errorf("read the backup's manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse the backup's manifest: %w", err)
	}
	return manifest, nil
}

// Verify compares what a restore observed with the manifest: every counted thing is present and
// within its range, and nothing restored is missing from the manifest. It returns the evidence (what
// was compared) and, on a mismatch, an error naming the first few differences.
func Verify(manifest Manifest, observed map[string]int64) (string, error) {
	var mismatches []string
	names := make([]string, 0, len(manifest.Summary))
	for name := range manifest.Summary {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		want := manifest.Summary[name]
		got, ok := observed[name]
		switch {
		case !ok:
			mismatches = append(mismatches, fmt.Sprintf("%s: missing", name))
		case got < want.Min || got > want.Max:
			mismatches = append(mismatches, fmt.Sprintf("%s: %d, backup had %s", name, got, want))
		}
		total += got
	}
	for name := range observed {
		if _, ok := manifest.Summary[name]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s: not in the backup", name))
		}
	}
	evidence := fmt.Sprintf("%d items compared with the manifest of %s, %d counted in total", len(names), manifest.Backup, total)
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		if len(mismatches) > 5 {
			mismatches = append(mismatches[:5], fmt.Sprintf("and %d more", len(mismatches)-5))
		}
		return evidence, fmt.Errorf("restored content does not match the backup: %s", strings.Join(mismatches, "; "))
	}
	return evidence, nil
}

func (c Count) String() string {
	if c.Min == c.Max {
		return fmt.Sprintf("%d", c.Min)
	}
	return fmt.Sprintf("%d to %d", c.Min, c.Max)
}

// RestoreMain runs `/manager restore ...` and returns the exit code. args excludes "restore".
func RestoreMain(args []string, getenv func(string) string, terminationLog string, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kind := flags.String("datastore", "", "the datastore kind (BackupDatastoreKind)")
	name := flags.String("restore", "", "the BackupRestore's name, for the log")
	prefix := flags.String("prefix", "", "the prefix in the bucket of the backup restored")
	verify := flags.Bool("verify", true, "compare the restored content with the backup's manifest")
	cleanup := flags.Bool("cleanup", false, "after verifying, delete what was restored (a scratch rehearsal target)")
	timeout := flags.Duration("timeout", 6*time.Hour, "how long the restore may take")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	report := restore(*kind, *name, *prefix, *verify, *cleanup, *timeout, getenv)
	if terminationLog != "" {
		_ = os.WriteFile(terminationLog, report.encode(), 0o644)
	}
	fmt.Fprintln(stderr, report.Message)
	if !report.Succeeded {
		return 1
	}
	return 0
}

func restore(kind, name, prefix string, verify, cleanup bool, timeout time.Duration, getenv func(string) string) Report {
	mechanism, ok := mechanisms[kind]
	if !ok {
		return Report{Message: fmt.Sprintf("no restore mechanism for datastore kind %q runs in a Job; supported: %s", kind, strings.Join(SupportedKinds(), ", "))}
	}
	if name == "" || prefix == "" {
		return Report{Message: "--restore and --prefix are required"}
	}
	bucket, store, err := fromEnvironment(getenv)
	if err != nil {
		return Report{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	source := Target{Bucket: bucket, Prefix: strings.Trim(prefix, "/") + "/"}
	manifest, err := ReadManifest(ctx, source)
	if err != nil {
		return Report{Message: redact(err, bucket.SecretAccessKey).Error()}
	}
	if manifest.Kind != kind {
		return Report{Message: fmt.Sprintf("the backup is of a %s datastore, not %s", manifest.Kind, kind)}
	}
	restored, err := mechanism.Restore(ctx, store, source, manifest)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Report{Message: fmt.Sprintf("restore %s did not finish within %s: %v", name, timeout, err)}
		}
		return Report{Message: redact(err, bucket.SecretAccessKey).Error()}
	}
	report := Report{Succeeded: true, Message: restored.Message}
	if report.Message == "" {
		report.Message = "restore completed"
	}
	if verify {
		evidence, err := Verify(manifest, restored.Observed)
		report.Evidence = evidence
		if err != nil {
			return Report{Message: err.Error(), Evidence: evidence}
		}
		report.Verified = true
		report.Message += "; " + evidence
	}
	if cleanup {
		cleaner, ok := mechanism.(Cleaner)
		if !ok {
			report.Message += "; the scratch target is left as restored (this kind cannot empty it)"
		} else if err := cleaner.Cleanup(ctx, store, manifest, restored); err != nil {
			report.Message += "; emptying the scratch target failed: " + err.Error()
		} else {
			report.Message += "; the scratch target was emptied"
		}
	}
	return report
}
