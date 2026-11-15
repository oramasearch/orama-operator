/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Valkey backs up an RDB snapshot of every primary to the bucket, over the replication protocol
// from Go (operator#736), so no Valkey tool is added to the operator image (Valkey is BSD-3-Clause).
//
// Backup: the Job finds the primaries (CLUSTER SHARDS; a server without cluster mode is its own only
// primary), asks each for a full resynchronisation (PSYNC ? -1), which makes the server fork and
// stream a point-in-time RDB of its data, and streams that RDB to <prefix>shard-<n>.rdb as a multipart
// upload while it counts the keys per database. A shard's snapshot is consistent; the shards are
// taken one after another, which is what a cluster's own replicas see too.
//
// Restore: into a server or cluster with no key at all. The Job reads each shard's RDB, checks its
// SHA-256 against the manifest, and writes every key with RESTORE (the value's own RDB encoding as a
// DUMP payload, its absolute expiry kept), to the primary that owns the key's slot in the target, so
// the target's topology may differ from the source's. A key that has expired, or expires within a
// minute, is counted but not written. Verified: the keys per database (DBSIZE summed over the
// primaries, plus those expired) equal the backup's. Cleanup (a scratch target): FLUSHALL on every
// primary, which held nothing before the restore.
type Valkey struct {
	// Dial opens a connection; nil means net.Dialer. Injected for tests.
	Dial func(ctx context.Context, address string) (net.Conn, error)
	// Now is the clock expiries are judged by; nil means time.Now.
	Now func() time.Time
}

// valkeyShardFile is one shard's RDB in a backup.
type valkeyShardFile struct {
	Key     string `json:"key"`
	Primary string `json:"primary"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	Version int    `json:"rdbVersion"`
}

const valkeyExpiryMargin = time.Minute

func (v Valkey) dial(ctx context.Context, address string) (net.Conn, error) {
	if v.Dial != nil {
		return v.Dial(ctx, address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", address)
}

func (v Valkey) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Backup implements Mechanism.
func (v Valkey) Backup(ctx context.Context, store Datastore, target Target) (Result, error) {
	primaries, err := v.primaries(ctx, store)
	if err != nil {
		return Result{}, err
	}
	s3 := &S3{Bucket: target.Bucket}
	totals := map[string]int64{}
	var files []valkeyShardFile
	var size int64
	for i, primary := range primaries {
		file, counts, err := v.backupPrimary(ctx, store, primary, s3, fmt.Sprintf("%sshard-%d.rdb", target.Prefix, i))
		if err != nil {
			return Result{}, fmt.Errorf("primary %s: %w", primary, err)
		}
		for db, n := range counts {
			totals[db] += n
		}
		files = append(files, file)
		size += file.Bytes
	}
	summary := map[string]Count{}
	for db, n := range totals {
		summary[db] = Exact(n)
	}
	extra, _ := json.Marshal(files)
	return Result{SizeBytes: size, Summary: summary, Extra: extra,
		Message: fmt.Sprintf("valkey backup of %d primaries, %d keys", len(primaries), sumCounts(totals))}, nil
}

func sumCounts(counts map[string]int64) int64 {
	var total int64
	for _, n := range counts {
		total += n
	}
	return total
}

// backupPrimary streams one primary's RDB to key, counting its keys as it passes.
func (v Valkey) backupPrimary(ctx context.Context, store Datastore, primary string, s3 *S3, key string) (valkeyShardFile, map[string]int64, error) {
	conn, err := v.open(ctx, store, primary)
	if err != nil {
		return valkeyShardFile{}, nil, err
	}
	defer conn.close()
	if err := conn.send("PSYNC", "?", "-1"); err != nil {
		return valkeyShardFile{}, nil, err
	}
	// +FULLRESYNC <replid> <offset>, then (after newlines the server sends while it forks) the RDB as
	// $<length>\r\n<bytes>.
	length := int64(-1)
	for length < 0 {
		line, err := conn.reader.ReadString('\n')
		if err != nil {
			return valkeyShardFile{}, nil, fmt.Errorf("read the synchronisation: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
		case strings.HasPrefix(line, "+FULLRESYNC"), strings.HasPrefix(line, "+CONTINUE"):
		case strings.HasPrefix(line, "-"):
			return valkeyShardFile{}, nil, fmt.Errorf("valkey refused PSYNC: %s", line[1:])
		case strings.HasPrefix(line, "$EOF:"):
			return valkeyShardFile{}, nil, errors.New("valkey sent a diskless EOF-delimited RDB, which it sends only to a replica that asked for it")
		case strings.HasPrefix(line, "$"):
			length, err = strconv.ParseInt(line[1:], 10, 64)
			if err != nil || length < 0 {
				return valkeyShardFile{}, nil, fmt.Errorf("valkey sent an RDB length %q", line)
			}
		default:
			return valkeyShardFile{}, nil, fmt.Errorf("valkey answered PSYNC with %q", line)
		}
	}
	hash := sha256.New()
	payload := io.TeeReader(io.LimitReader(conn.reader, length), hash)
	pipeReader, pipeWriter := io.Pipe()
	uploaded := make(chan error, 1)
	var written int64
	go func() {
		n, err := s3.PutStream(ctx, key, pipeReader)
		written = n
		_ = pipeReader.CloseWithError(err)
		uploaded <- err
	}()
	counts := map[string]int64{}
	version, parseErr := readRDB(io.TeeReader(payload, pipeWriter), func(e rdbEntry) error {
		counts[fmt.Sprintf("db%d", e.db)]++
		return nil
	})
	if parseErr == nil {
		// Whatever follows the EOF marker (nothing, normally) still goes to the bucket.
		_, parseErr = io.Copy(pipeWriter, payload)
	}
	_ = pipeWriter.CloseWithError(parseErr)
	if err := <-uploaded; err != nil && parseErr == nil {
		return valkeyShardFile{}, nil, fmt.Errorf("upload %s: %w", key, err)
	}
	if parseErr != nil {
		return valkeyShardFile{}, nil, fmt.Errorf("read the RDB: %w", parseErr)
	}
	return valkeyShardFile{Key: key, Primary: primary, Bytes: written, SHA256: hex.EncodeToString(hash.Sum(nil)), Version: version}, counts, nil
}

// Restore implements Mechanism.
func (v Valkey) Restore(ctx context.Context, store Datastore, source Target, manifest Manifest) (Restored, error) {
	var files []valkeyShardFile
	if err := json.Unmarshal(manifest.Extra, &files); err != nil || len(files) == 0 {
		return Restored{}, fmt.Errorf("the manifest lists no shard files")
	}
	router, err := v.route(ctx, store)
	if err != nil {
		return Restored{}, err
	}
	defer router.close()
	for _, total := range router.dbsize(ctx) {
		if total != 0 {
			return Restored{}, errors.New("the target holds keys already: a restore goes into an empty server")
		}
	}
	if router.err != nil {
		return Restored{}, router.err
	}
	s3 := &S3{Bucket: source.Bucket}
	expired := map[string]int64{}
	restored := int64(0)
	cutoff := v.now().Add(valkeyExpiryMargin).UnixMilli()
	for _, file := range files {
		body, err := s3.GetStream(ctx, file.Key)
		if err != nil {
			return Restored{}, err
		}
		hash := sha256.New()
		_, err = readRDB(io.TeeReader(body, hash), func(e rdbEntry) error {
			db := fmt.Sprintf("db%d", e.db)
			if e.expireMS > 0 && e.expireMS <= cutoff {
				expired[db]++
				return nil
			}
			restored++
			return router.restore(e)
		})
		_, _ = io.Copy(hash, body)
		body.Close()
		if err == nil {
			err = router.flush()
		}
		if err != nil {
			return Restored{}, fmt.Errorf("restore %s: %w", file.Key, err)
		}
		if sum := hex.EncodeToString(hash.Sum(nil)); sum != file.SHA256 {
			return Restored{}, fmt.Errorf("%s does not match its checksum in the manifest", file.Key)
		}
	}
	observed := router.dbsize(ctx)
	if router.err != nil {
		return Restored{}, router.err
	}
	for db, n := range expired {
		observed[db] += n
	}
	message := fmt.Sprintf("valkey restore of %d shard files, %d keys written", len(files), restored)
	if n := sumCounts(expired); n > 0 {
		message += fmt.Sprintf(", %d expired keys not written", n)
	}
	return Restored{Observed: observed, Message: message}, nil
}

// Cleanup implements Cleaner: the scratch target held nothing before the restore.
func (v Valkey) Cleanup(ctx context.Context, store Datastore, _ Manifest, _ Restored) error {
	primaries, err := v.primaries(ctx, store)
	if err != nil {
		return err
	}
	for _, primary := range primaries {
		conn, err := v.open(ctx, store, primary)
		if err != nil {
			return err
		}
		_, err = conn.do("FLUSHALL", "SYNC")
		conn.close()
		if err != nil {
			return err
		}
	}
	return nil
}

// primaries are host:port of every primary: from CLUSTER SHARDS, or the server itself.
func (v Valkey) primaries(ctx context.Context, store Datastore) ([]string, error) {
	shards, err := v.shards(ctx, store)
	if err != nil {
		return nil, err
	}
	primaries := make([]string, 0, len(shards))
	for _, shard := range shards {
		primaries = append(primaries, shard.primary)
	}
	return primaries, nil
}

type valkeyShard struct {
	primary string
	slots   [][2]int
}

func (v Valkey) address(store Datastore) string {
	port := store.Port
	if port == "" {
		port = "6379"
	}
	return net.JoinHostPort(store.Host, port)
}

func (v Valkey) shards(ctx context.Context, store Datastore) ([]valkeyShard, error) {
	conn, err := v.open(ctx, store, v.address(store))
	if err != nil {
		return nil, err
	}
	defer conn.close()
	reply, err := conn.do("CLUSTER", "SHARDS")
	if err != nil {
		if strings.Contains(err.Error(), "cluster support disabled") || strings.Contains(err.Error(), "unknown") {
			return []valkeyShard{{primary: v.address(store), slots: [][2]int{{0, 16383}}}}, nil
		}
		return nil, err
	}
	var shards []valkeyShard
	for _, item := range asArray(reply) {
		fields := asMap(item)
		shard := valkeyShard{}
		slots := asArray(fields["slots"])
		for i := 0; i+1 < len(slots); i += 2 {
			start, _ := asInt(slots[i])
			end, _ := asInt(slots[i+1])
			shard.slots = append(shard.slots, [2]int{int(start), int(end)})
		}
		for _, node := range asArray(fields["nodes"]) {
			n := asMap(node)
			role, _ := n["role"].(string)
			health, _ := n["health"].(string)
			if (role != "master" && role != "primary") || health != "online" {
				continue
			}
			ip, _ := n["ip"].(string)
			if ip == "" {
				ip, _ = n["endpoint"].(string)
			}
			port, ok := asInt(n["port"])
			if !ok {
				port, _ = asInt(n["tls-port"])
			}
			shard.primary = net.JoinHostPort(ip, strconv.FormatInt(port, 10))
		}
		if shard.primary == "" || len(shard.slots) == 0 {
			continue
		}
		shards = append(shards, shard)
	}
	if len(shards) == 0 {
		return nil, errors.New("CLUSTER SHARDS lists no online primary that owns slots")
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i].slots[0][0] < shards[j].slots[0][0] })
	return shards, nil
}

// valkeyRouter writes RESTOREs to the primary owning each key's slot, pipelined.
type valkeyRouter struct {
	v       Valkey
	store   Datastore
	shards  []valkeyShard
	conns   map[string]*respConn
	pending map[string]int
	cluster bool
	err     error
}

const valkeyPipeline = 512

func (v Valkey) route(ctx context.Context, store Datastore) (*valkeyRouter, error) {
	shards, err := v.shards(ctx, store)
	if err != nil {
		return nil, err
	}
	router := &valkeyRouter{v: v, store: store, shards: shards, conns: map[string]*respConn{}, pending: map[string]int{}, cluster: len(shards) > 1 || shards[0].primary != v.address(store)}
	for _, shard := range shards {
		conn, err := v.open(ctx, store, shard.primary)
		if err != nil {
			router.close()
			return nil, err
		}
		router.conns[shard.primary] = conn
	}
	return router, nil
}

func (r *valkeyRouter) close() {
	for _, conn := range r.conns {
		conn.close()
	}
}

func (r *valkeyRouter) owner(key []byte) string {
	slot := valkeyKeySlot(key)
	for _, shard := range r.shards {
		for _, span := range shard.slots {
			if slot >= span[0] && slot <= span[1] {
				return shard.primary
			}
		}
	}
	return r.shards[0].primary
}

func (r *valkeyRouter) restore(e rdbEntry) error {
	primary := r.owner(e.key)
	conn := r.conns[primary]
	if !r.cluster && e.db != conn.db {
		if err := r.drain(primary); err != nil {
			return err
		}
		if _, err := conn.do("SELECT", strconv.Itoa(e.db)); err != nil {
			return err
		}
		conn.db = e.db
	}
	args := []string{"RESTORE", string(e.key), "0", string(e.dumpPayload())}
	if e.expireMS > 0 {
		args = []string{"RESTORE", string(e.key), strconv.FormatInt(e.expireMS, 10), string(e.dumpPayload()), "ABSTTL"}
	}
	if err := conn.send(args...); err != nil {
		return err
	}
	r.pending[primary]++
	if r.pending[primary] >= valkeyPipeline {
		return r.drain(primary)
	}
	return nil
}

func (r *valkeyRouter) drain(primary string) error {
	conn := r.conns[primary]
	if err := conn.writer.Flush(); err != nil {
		return err
	}
	for ; r.pending[primary] > 0; r.pending[primary]-- {
		if _, err := conn.read(); err != nil {
			return err
		}
	}
	return nil
}

func (r *valkeyRouter) flush() error {
	for primary := range r.conns {
		if err := r.drain(primary); err != nil {
			return err
		}
	}
	return nil
}

// dbsize is the keys per database over every primary.
func (r *valkeyRouter) dbsize(_ context.Context) map[string]int64 {
	totals := map[string]int64{}
	for _, conn := range r.conns {
		reply, err := conn.do("INFO", "keyspace")
		if err != nil {
			r.err = err
			return totals
		}
		text, _ := reply.(string)
		for _, line := range strings.Split(text, "\n") {
			name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || !strings.HasPrefix(name, "db") {
				continue
			}
			for _, field := range strings.Split(rest, ",") {
				if value, ok := strings.CutPrefix(field, "keys="); ok {
					n, _ := strconv.ParseInt(value, 10, 64)
					totals[name] += n
				}
			}
		}
	}
	return totals
}

// valkeyKeySlot is the cluster slot of a key: CRC16 (XMODEM) of its hash tag, or of the key.
func valkeyKeySlot(key []byte) int {
	if start := indexByte(key, '{'); start >= 0 {
		if end := indexByte(key[start+1:], '}'); end > 0 {
			key = key[start+1 : start+1+end]
		}
	}
	var crc uint16
	for _, b := range key {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return int(crc) & 16383
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// respConn is a minimal RESP2 client: enough for AUTH, PSYNC, CLUSTER SHARDS, INFO, RESTORE.
type respConn struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
	db     int
}

func (v Valkey) open(ctx context.Context, store Datastore, address string) (*respConn, error) {
	conn, err := v.dial(ctx, address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c := &respConn{conn: conn, reader: bufio.NewReaderSize(conn, 1<<20), writer: bufio.NewWriter(conn)}
	if store.Password != "" {
		args := []string{"AUTH", store.Password}
		if store.Username != "" {
			args = []string{"AUTH", store.Username, store.Password}
		}
		if _, err := c.do(args...); err != nil {
			c.close()
			return nil, fmt.Errorf("valkey refused AUTH: %w", redact(err, store.Password))
		}
	}
	return c, nil
}

func (c *respConn) close() { _ = c.conn.Close() }

func (c *respConn) send(args ...string) error {
	fmt.Fprintf(c.writer, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(c.writer, "$%d\r\n", len(arg))
		c.writer.WriteString(arg)
		c.writer.WriteString("\r\n")
	}
	return nil
}

func (c *respConn) do(args ...string) (any, error) {
	if err := c.send(args...); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}
	return c.read()
}

// read reads one reply. An error reply is returned as an error.
func (c *respConn) read() (any, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("valkey sent an empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, fmt.Errorf("valkey: %s", line[1:])
	case ':':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		return n, err
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		data := make([]byte, n+2)
		if _, err := io.ReadFull(c.reader, data); err != nil {
			return nil, err
		}
		return string(data[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		items := make([]any, n)
		for i := range items {
			if items[i], err = c.read(); err != nil {
				return nil, err
			}
		}
		return items, nil
	}
	return nil, fmt.Errorf("valkey sent %q", line)
}

func asArray(v any) []any {
	items, _ := v.([]any)
	return items
}

// asMap reads a RESP2 flat key-value array.
func asMap(v any) map[string]any {
	items := asArray(v)
	out := map[string]any{}
	for i := 0; i+1 < len(items); i += 2 {
		if key, ok := items[i].(string); ok {
			out[key] = items[i+1]
		}
	}
	return out
}

func asInt(v any) (int64, bool) {
	switch value := v.(type) {
	case int64:
		return value, true
	case string:
		n, err := strconv.ParseInt(value, 10, 64)
		return n, err == nil
	}
	return 0, false
}
