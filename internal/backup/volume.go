/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Volume copies a volume claim file by file to the bucket (operator#738): the controller takes a CSI
// VolumeSnapshot of the claim and mounts a claim made from it, read-only, at VOLUME_PATH in the Job,
// so the copy is of one crash-consistent moment and the workload keeps running. The Job writes one
// gzip-compressed tar, volume.tar.gz, as a multipart upload, recording every regular file's size and
// SHA-256 in a digest. A restore goes into an empty claim mounted at VOLUME_PATH, refuses any path
// that would leave it, writes symbolic links last so nothing is written through one, and is verified
// by the files, bytes and digest it wrote.
//
// The same mechanism backs up the wrapper services (operator#737): SQLite and DuckDB recover a
// committed state from exactly such a crash-consistent image (their journal and write-ahead log are
// in the snapshot with the database), and Polars writes each dataset by rename, so a snapshot never
// holds half of one. For those kinds Format also counts the database or dataset files whose header
// is that engine's, before the backup and after the restore.
type Volume struct {
	// Format names the files the kind keeps, and checks one's header; nil for a plain volume.
	Format *VolumeFormat
}

// VolumeFormat is a wrapper kind's data files.
type VolumeFormat struct {
	Name  string
	Match func(path string) bool
	Valid func(head, tail []byte) bool
}

const volumeArchiveKey = "volume.tar.gz"

// volumeDigest is what a volume backup records in its manifest.
type volumeDigest struct {
	Digest string `json:"digest"`
}

var (
	// SQLiteFormat: a database file begins "SQLite format 3\0".
	SQLiteFormat = &VolumeFormat{Name: "sqlite databases",
		Match: func(path string) bool { return hasSuffix(path, ".db", ".sqlite", ".sqlite3") },
		Valid: func(head, _ []byte) bool { return bytes.HasPrefix(head, []byte("SQLite format 3\x00")) }}
	// DuckDBFormat: a database file has "DUCK" at byte 8.
	DuckDBFormat = &VolumeFormat{Name: "duckdb databases",
		Match: func(path string) bool { return hasSuffix(path, ".duckdb", ".db") },
		Valid: func(head, _ []byte) bool { return len(head) >= 12 && string(head[8:12]) == "DUCK" }}
	// PolarsFormat: a dataset is a Parquet file, "PAR1" at both ends.
	PolarsFormat = &VolumeFormat{Name: "parquet datasets",
		Match: func(path string) bool { return hasSuffix(path, ".parquet") },
		Valid: func(head, tail []byte) bool {
			return bytes.HasPrefix(head, []byte("PAR1")) && bytes.HasSuffix(tail, []byte("PAR1"))
		}}
)

func hasSuffix(path string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// volumeTally is what a walk of a volume counted.
type volumeTally struct {
	files, bytes, valid, invalid int64
	lines                        []string
}

func (t *volumeTally) add(format *VolumeFormat, path string, size int64, sum string, head, tail []byte) {
	t.files++
	t.bytes += size
	t.lines = append(t.lines, fmt.Sprintf("%s\x00%d\x00%s", path, size, sum))
	if format != nil && format.Match(path) {
		if format.Valid(head, tail) {
			t.valid++
		} else {
			t.invalid++
		}
	}
}

func (t *volumeTally) digest() string {
	sort.Strings(t.lines)
	sum := sha256.Sum256([]byte(strings.Join(t.lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func (t *volumeTally) summary(format *VolumeFormat) map[string]Count {
	summary := map[string]Count{"files": Exact(t.files), "bytes": Exact(t.bytes)}
	if format != nil {
		summary[format.Name] = Exact(t.valid)
	}
	return summary
}

// hashingFile reads a file through SHA-256, keeping its first and last 16 bytes.
type hashingFile struct {
	hash       io.Writer
	head, tail []byte
}

func (h *hashingFile) Write(p []byte) (int, error) {
	if len(h.head) < 16 {
		h.head = append(h.head, p[:min(len(p), 16-len(h.head))]...)
	}
	h.tail = append(h.tail, p...)
	if len(h.tail) > 16 {
		h.tail = append([]byte(nil), h.tail[len(h.tail)-16:]...)
	}
	return h.hash.Write(p)
}

func volumeRoot() (string, error) {
	root := os.Getenv(EnvVolumePath)
	if root == "" {
		return "", errors.New("no volume is mounted (VOLUME_PATH is empty)")
	}
	return root, nil
}

// Backup implements Mechanism.
func (v Volume) Backup(ctx context.Context, _ Datastore, target Target) (Result, error) {
	root, err := volumeRoot()
	if err != nil {
		return Result{}, err
	}
	pipeReader, pipeWriter := io.Pipe()
	uploaded := make(chan error, 1)
	var size int64
	go func() {
		n, err := (&S3{Bucket: target.Bucket}).PutStream(ctx, target.Prefix+volumeArchiveKey, pipeReader)
		size = n
		_ = pipeReader.CloseWithError(err)
		uploaded <- err
	}()
	tally, walkErr := v.archive(ctx, root, pipeWriter)
	_ = pipeWriter.CloseWithError(walkErr)
	if err := <-uploaded; err != nil && walkErr == nil {
		return Result{}, fmt.Errorf("upload the archive: %w", err)
	}
	if walkErr != nil {
		return Result{}, walkErr
	}
	if tally.invalid > 0 {
		return Result{}, fmt.Errorf("%d of the volume's %s do not have their engine's header", tally.invalid, v.Format.Name)
	}
	extra, _ := json.Marshal(volumeDigest{Digest: tally.digest()})
	return Result{SizeBytes: size, Summary: tally.summary(v.Format), Extra: extra,
		Message: fmt.Sprintf("volume copy: %d files, %d bytes, %d compressed", tally.files, tally.bytes, size)}, nil
}

// archive writes root as a gzip-compressed tar to w.
func (v Volume) archive(ctx context.Context, root string, w io.Writer) (*volumeTally, error) {
	compressed := gzip.NewWriter(w)
	archive := tar.NewWriter(compressed)
	tally := &volumeTally{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "lost+found" && entry.IsDir() {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, pipes and devices are not data
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		tee := &hashingFile{hash: hash}
		n, err := io.Copy(archive, io.TeeReader(file, tee))
		if err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		tally.add(v.Format, rel, n, hex.EncodeToString(hash.Sum(nil)), tee.head, tee.tail)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return tally, compressed.Close()
}

// Restore implements Mechanism.
func (v Volume) Restore(ctx context.Context, _ Datastore, source Target, manifest Manifest) (Restored, error) {
	root, err := volumeRoot()
	if err != nil {
		return Restored{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Restored{}, err
	}
	for _, entry := range entries {
		if entry.Name() != "lost+found" {
			return Restored{}, fmt.Errorf("the target volume is not empty (%s): a restore goes into an empty volume", entry.Name())
		}
	}
	body, err := (&S3{Bucket: source.Bucket}).GetStream(ctx, source.Prefix+volumeArchiveKey)
	if err != nil {
		return Restored{}, err
	}
	defer body.Close()
	tally, err := v.extract(root, body)
	if err != nil {
		return Restored{}, err
	}
	var recorded volumeDigest
	_ = json.Unmarshal(manifest.Extra, &recorded)
	if recorded.Digest != "" && recorded.Digest != tally.digest() {
		return Restored{}, errors.New("the restored files' digest differs from the backup's: a file's content or size changed")
	}
	observed := map[string]int64{}
	for name, count := range tally.summary(v.Format) {
		observed[name] = count.Min
	}
	return Restored{Observed: observed, Message: fmt.Sprintf("volume restore: %d files, %d bytes, digest matched", tally.files, tally.bytes)}, nil
}

// extract writes the archive under root. A path that is absolute or leaves root is refused, and
// symbolic links are created only after every file, so no file is written through one.
func (v Volume) extract(root string, r io.Reader) (*volumeTally, error) {
	decompressed, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("read the archive: %w", err)
	}
	archive := tar.NewReader(decompressed)
	tally := &volumeTally{}
	type link struct{ path, target string }
	var links []link
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("the archive names %q, outside the volume", header.Name)
		}
		path := filepath.Join(root, name)
		mode := fs.FileMode(header.Mode).Perm()
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, mode|0o700); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			links = append(links, link{path, header.Linkname})
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600)
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			tee := &hashingFile{hash: hash}
			n, err := io.Copy(file, io.TeeReader(archive, tee))
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return nil, fmt.Errorf("write %s: %v %v", name, err, closeErr)
			}
			_ = os.Chtimes(path, time.Now(), header.ModTime)
			tally.add(v.Format, filepath.ToSlash(name), n, hex.EncodeToString(hash.Sum(nil)), tee.head, tee.tail)
		}
	}
	for _, l := range links {
		if err := os.Symlink(l.target, l.path); err != nil {
			return nil, err
		}
	}
	return tally, nil
}
