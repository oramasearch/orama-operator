/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCRC64JonesMatchesValkeysCheckValue(t *testing.T) {
	// Valkey's src/crc64.c test: crc64(0, "123456789") == 0xe9c6d914c4b8d9ca.
	if got := crc64Jones(0, []byte("123456789")); got != 0xe9c6d914c4b8d9ca {
		t.Fatalf("crc64 = %#x", got)
	}
}

// rdbString encodes a plain length-prefixed string (lengths under 64).
func rdbString(s string) []byte { return append([]byte{byte(len(s))}, s...) }

// testRDB is a small RDB: an AUX field, db 0 with a string that expires and a list, db 2 with a
// string, the EOF marker and a checksum.
func testRDB(expireMS int64) []byte {
	var b bytes.Buffer
	b.WriteString("REDIS0011")
	b.WriteByte(rdbOpAux)
	b.Write(rdbString("redis-ver"))
	b.Write(rdbString("8.0.0"))
	b.Write([]byte{rdbOpSelectDB, 0, rdbOpResizeDB, 2, 1})
	b.WriteByte(rdbOpExpireMS)
	_ = binary.Write(&b, binary.LittleEndian, uint64(expireMS))
	b.WriteByte(0)
	b.Write(rdbString("session"))
	b.Write(rdbString("abc"))
	b.WriteByte(1)
	b.Write(rdbString("queue"))
	b.WriteByte(2)
	b.Write(rdbString("a"))
	b.Write(rdbString("b"))
	b.Write([]byte{rdbOpSelectDB, 2})
	b.WriteByte(0)
	b.Write(rdbString("n"))
	b.Write([]byte{0xC0, 42}) // an int8-encoded string
	b.WriteByte(rdbOpEOF)
	_ = binary.Write(&b, binary.LittleEndian, crc64Jones(0, b.Bytes()))
	return b.Bytes()
}

func TestReadRDBCountsEveryKeyAndKeepsItsEncoding(t *testing.T) {
	var entries []rdbEntry
	version, err := readRDB(bytes.NewReader(testRDB(1)), func(e rdbEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil || version != 11 || len(entries) != 3 {
		t.Fatalf("version %d, %d entries, %v", version, len(entries), err)
	}
	if string(entries[0].key) != "session" || entries[0].expireMS != 1 || entries[1].typ != 1 || entries[2].db != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	// The value is kept as written: the int8 encoding, not "42".
	if !bytes.Equal(entries[2].value, []byte{0xC0, 42}) {
		t.Fatalf("value = %x", entries[2].value)
	}
	payload := entries[2].dumpPayload()
	if payload[0] != 0 || binary.LittleEndian.Uint16(payload[3:5]) != 11 ||
		binary.LittleEndian.Uint64(payload[5:]) != crc64Jones(0, payload[:5]) {
		t.Fatalf("dump payload = %x", payload)
	}
	if _, err := readRDB(bytes.NewReader(testRDB(1)[:40]), func(rdbEntry) error { return nil }); err == nil {
		t.Fatal("a cut RDB was read")
	}
}

func TestLZFDecompress(t *testing.T) {
	// "aaaaaaaaaa": a literal "a", then a back reference of length 9 at distance 1.
	got, err := lzfDecompress([]byte{0x00, 'a', 0xE0, 0x00, 0x00}, 10)
	if err != nil || string(got) != "aaaaaaaaaa" {
		t.Fatalf("lzf = %q, %v", got, err)
	}
}

// fakeValkey is one Valkey server without cluster mode: it answers AUTH, CLUSTER SHARDS (refused),
// PSYNC with an RDB, INFO keyspace, RESTORE and FLUSHALL.
type fakeValkey struct {
	mu       sync.Mutex
	rdb      []byte
	keys     map[int]int
	restores []string
	flushed  bool
}

func (f *fakeValkey) serve(t *testing.T) (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return listener.Addr().String(), func() { listener.Close() }
}

func (f *fakeValkey) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	db := 0
	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		f.mu.Lock()
		switch strings.ToUpper(args[0]) {
		case "AUTH":
			if args[len(args)-1] != "pw" {
				fmt.Fprint(conn, "-WRONGPASS invalid username-password pair\r\n")
			} else {
				fmt.Fprint(conn, "+OK\r\n")
			}
		case "CLUSTER":
			fmt.Fprint(conn, "-ERR This instance has cluster support disabled\r\n")
		case "PSYNC":
			fmt.Fprintf(conn, "+FULLRESYNC 0123456789abcdef0123456789abcdef01234567 0\r\n\n$%d\r\n", len(f.rdb))
			conn.Write(f.rdb)
			f.mu.Unlock()
			return
		case "SELECT":
			db, _ = strconv.Atoi(args[1])
			fmt.Fprint(conn, "+OK\r\n")
		case "RESTORE":
			f.restores = append(f.restores, fmt.Sprintf("%d/%s", db, args[1]))
			if f.keys == nil {
				f.keys = map[int]int{}
			}
			f.keys[db]++
			fmt.Fprint(conn, "+OK\r\n")
		case "INFO":
			var info strings.Builder
			info.WriteString("# Keyspace\r\n")
			for n, keys := range f.keys {
				fmt.Fprintf(&info, "db%d:keys=%d,expires=0,avg_ttl=0\r\n", n, keys)
			}
			fmt.Fprintf(conn, "$%d\r\n%s\r\n", info.Len(), info.String())
		case "FLUSHALL":
			f.flushed, f.keys = true, nil
			fmt.Fprint(conn, "+OK\r\n")
		default:
			fmt.Fprintf(conn, "-ERR unknown command '%s'\r\n", args[0])
		}
		f.mu.Unlock()
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(strings.TrimSpace(header[1:]))
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}

// A backup streams the RDB to the bucket and counts its keys; a restore into an empty server writes
// each key to its database, skips an expired one, and verifies.
func TestValkeyBacksUpAndRestoresThroughTheReplicationProtocol(t *testing.T) {
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	source := &fakeValkey{rdb: testRDB(now.Add(-time.Hour).UnixMilli())}
	address, stop := source.serve(t)
	defer stop()
	host, port, _ := net.SplitHostPort(address)
	store := Datastore{Host: host, Port: port, Username: "app", Password: "pw"}

	objects := &fakeStore{objects: map[string][]byte{}, pageMax: 10}
	bucketServer := httptest.NewServer(objects)
	defer bucketServer.Close()
	target := Target{Bucket: Bucket{Endpoint: bucketServer.URL, Name: "backups", Region: "auto", AccessKeyID: "key-id", SecretAccessKey: "s"}, Prefix: "ws-1/valkey/cache/b1/"}

	mechanism := Valkey{Now: func() time.Time { return now }}
	result, err := mechanism.Backup(context.Background(), store, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(objects.objects["ws-1/valkey/cache/b1/shard-0.rdb"], source.rdb) {
		t.Fatalf("uploaded RDB differs: %d bytes, want %d", len(objects.objects["ws-1/valkey/cache/b1/shard-0.rdb"]), len(source.rdb))
	}
	if result.Summary["db0"] != Exact(2) || result.Summary["db2"] != Exact(1) || result.SizeBytes != int64(len(source.rdb)) {
		t.Fatalf("result = %+v", result)
	}
	manifest := Manifest{Kind: "Valkey", Backup: "b1", Summary: result.Summary, Extra: result.Extra}

	scratch := &fakeValkey{}
	scratchAddress, stopScratch := scratch.serve(t)
	defer stopScratch()
	scratchHost, scratchPort, _ := net.SplitHostPort(scratchAddress)
	scratchStore := Datastore{Host: scratchHost, Port: scratchPort, Username: "app", Password: "pw"}
	restored, err := mechanism.Restore(context.Background(), scratchStore, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	scratch.mu.Lock()
	restores := strings.Join(scratch.restores, ",")
	scratch.mu.Unlock()
	if restores != "0/queue,2/n" {
		t.Fatalf("restored %v; want queue and n, not the expired session", scratch.restores)
	}
	if _, err := Verify(manifest, restored.Observed); err != nil {
		t.Fatalf("verify: %v (observed %v)", err, restored.Observed)
	}
	if err := mechanism.Cleanup(context.Background(), scratchStore, manifest); err != nil || !scratch.flushed {
		t.Fatalf("cleanup: %v, flushed %v", err, scratch.flushed)
	}

	// A target that holds keys is refused.
	source.mu.Lock()
	source.keys = map[int]int{0: 1}
	source.mu.Unlock()
	if _, err := mechanism.Restore(context.Background(), store, target, manifest); err == nil || !strings.Contains(err.Error(), "holds keys") {
		t.Fatalf("restore into a server with keys: %v", err)
	}
}

func TestValkeyKeySlotUsesTheHashTag(t *testing.T) {
	// CLUSTER KEYSLOT foo = 12182; {user1000}.following and {user1000}.followers share a slot.
	if got := valkeyKeySlot([]byte("foo")); got != 12182 {
		t.Fatalf("slot(foo) = %d", got)
	}
	if valkeyKeySlot([]byte("{user1000}.following")) != valkeyKeySlot([]byte("{user1000}.followers")) {
		t.Fatal("hash tags differ")
	}
}
