/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCacheACLAgainstRedis loads the rendered ACL file into a real redis-server (#825) and checks
// that each client user can run its own commands on its own keys and is refused everything else:
// another client's keys, FLUSHALL, CONFIG, ACL, MODULE, DEBUG; that the default user is off; and
// that the admin user is unrestricted. Opt-in: CACHE_ACL_TEST_REDIS_SERVER names a redis-server
// binary (7.x, the cache image's major version).
func TestCacheACLAgainstRedis(t *testing.T) {
	server := os.Getenv("CACHE_ACL_TEST_REDIS_SERVER")
	if server == "" {
		t.Skip("CACHE_ACL_TEST_REDIS_SERVER is required for the Redis ACL proof")
	}
	passwords := map[string]string{redisPasswordEnv: "adminproof0123"}
	for _, c := range cacheClients {
		passwords[cacheClientPasswordEnv(c.user)] = c.user + "-proof0123"
	}
	acl := cacheACLTemplate()
	for name, value := range passwords {
		acl = strings.ReplaceAll(acl, keyPlaceholder(name), value)
	}
	dir := t.TempDir()
	aclPath := filepath.Join(dir, cacheACLFile)
	if err := os.WriteFile(aclPath, []byte(acl), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := exec.Command(server, "--port", strconv.Itoa(port), "--bind", "127.0.0.1", "--dir", dir,
		"--appendonly", "yes", "--aclfile", aclPath, "--save", "")
	if out, err := cmd.StdoutPipe(); err == nil {
		go func() { _, _ = bufio.NewReader(out).WriteTo(os.Stderr) }()
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var conn net.Conn
	for i := 0; i < 50; i++ {
		if conn, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("redis-server did not start: %v", err)
	}
	_ = conn.Close()

	as := func(user, pass string) *respConn {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		r := &respConn{t: t, c: c, r: bufio.NewReader(c)}
		if user != "" {
			if got := r.do("AUTH", user, pass); got != "+OK" {
				t.Fatalf("AUTH %s: %s", user, got)
			}
		}
		return r
	}
	allowed := func(r *respConn, args ...string) {
		t.Helper()
		if got := r.do(args...); strings.HasPrefix(got, "-") {
			t.Errorf("%v refused: %s", args, got)
		}
	}
	refused := func(r *respConn, args ...string) {
		t.Helper()
		if got := r.do(args...); !strings.HasPrefix(got, "-NOPERM") && !strings.HasPrefix(got, "-NOAUTH") && !strings.HasPrefix(got, "-WRONGPASS") {
			t.Errorf("%v allowed: %s", args, got)
		}
	}

	// The default user is off: no command without AUTH, and AUTH with no user fails.
	anon := as("", "")
	refused(anon, "GET", "v1:x")
	refused(anon, "AUTH", passwords[redisPasswordEnv])

	ib := as(cacheClientIndexBuilder, passwords[cacheClientPasswordEnv(cacheClientIndexBuilder)])
	allowed(ib, "PING")
	allowed(ib, "MSET", "v1:a", "1", "v1:b", "2")
	allowed(ib, "MGET", "v1:a", "v1:b")
	refused(ib, "MGET", "job:1")
	refused(ib, "GET", "v1:a")
	refused(ib, "KEYS", "*")

	hm := as(cacheClientHarvestMaster, passwords[cacheClientPasswordEnv(cacheClientHarvestMaster)])
	allowed(hm, "SET", "job:1", "{}")
	allowed(hm, "SET", "heartbeat:1", "t", "EX", "60")
	allowed(hm, "GET", "job:1")
	allowed(hm, "KEYS", "job:*")
	allowed(hm, "DEL", "job:1")
	refused(hm, "GET", "v1:a")
	refused(hm, "SET", "project:1", "x")

	rh := as(cacheClientRedisHTTP, passwords[cacheClientPasswordEnv(cacheClientRedisHTTP)])
	allowed(rh, "SET", "project:1:search-analytics:a:b:c", "{}", "EX", "43200")
	allowed(rh, "GET", "project:1:search-analytics:a:b:c")
	refused(rh, "GET", "job:1")
	refused(rh, "DEL", "project:1:search-analytics:a:b:c")

	for _, r := range []*respConn{ib, hm, rh} {
		for _, args := range [][]string{
			{"FLUSHALL"}, {"FLUSHDB"}, {"CONFIG", "GET", "dir"}, {"CONFIG", "SET", "dir", "/tmp"},
			{"ACL", "LIST"}, {"MODULE", "LIST"}, {"DEBUG", "SLEEP", "0"}, {"CLIENT", "KILL", "ID", "1"},
			{"EVAL", "return 1", "0"}, {"SHUTDOWN"},
		} {
			refused(r, args...)
		}
	}

	admin := as(cacheAdminUser, passwords[redisPasswordEnv])
	allowed(admin, "ACL", "LIST")
	allowed(admin, "GET", "v1:a")
}

// respConn is a minimal RESP2 client for the proof: it sends a command and returns the reply's
// first line (+OK, -ERR ..., :1, $n, *n), reading and discarding any bulk payload.
type respConn struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (r *respConn) do(args ...string) string {
	r.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := r.c.Write([]byte(b.String())); err != nil {
		r.t.Fatal(err)
	}
	first := r.line()
	r.skip(first)
	return first
}

func (r *respConn) line() string {
	r.t.Helper()
	s, err := r.r.ReadString('\n')
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.TrimRight(s, "\r\n")
}

// skip reads the rest of a reply whose first line is first.
func (r *respConn) skip(first string) {
	if first == "" {
		return
	}
	n, err := strconv.Atoi(first[1:])
	switch first[0] {
	case '$':
		if err == nil && n >= 0 {
			buf := make([]byte, n+2)
			if _, err := io.ReadFull(r.r, buf); err != nil {
				r.t.Fatal(err)
			}
		}
	case '*':
		for i := 0; err == nil && i < n; i++ {
			r.skip(r.line())
		}
	}
}
