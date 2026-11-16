// SPDX-License-Identifier: MIT

package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The "GET Bucket (List Objects)" example of AWS's Signature Version 4 documentation
// (https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html).
func TestSigV4MatchesTheAWSExample(t *testing.T) {
	s := &S3{
		Bucket: Bucket{Region: "us-east-1", AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		Now:    func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.sign(req, nil)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	pageMax int
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key-id/") {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/backups/")
	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/backups":
		prefix := r.URL.Query().Get("prefix")
		after := r.URL.Query().Get("continuation-token")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) && k > after {
				keys = append(keys, k)
			}
		}
		sortStrings(keys)
		truncated := len(keys) > f.pageMax
		if truncated {
			keys = keys[:f.pageMax]
		}
		var b bytes.Buffer
		b.WriteString("<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.objects[k]))
		}
		if truncated {
			fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
		}
		b.WriteString("</ListBucketResult>")
		_, _ = w.Write(b.Bytes())
	default:
		http.Error(w, "unexpected", http.StatusBadRequest)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestS3ListPagesAndDeletePrefixKeepsOtherBackups(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{}, pageMax: 2}
	server := httptest.NewServer(store)
	defer server.Close()
	s := &S3{Bucket: Bucket{Endpoint: server.URL, Name: "backups", Region: "auto", AccessKeyID: "key-id", SecretAccessKey: "secret"}}
	ctx := context.Background()
	for _, key := range []string{"ns/clickhouse/db/b1/a", "ns/clickhouse/db/b1/b", "ns/clickhouse/db/b1/c", "ns/clickhouse/db/b2/a"} {
		if err := s.Put(ctx, key, []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := s.List(ctx, "ns/clickhouse/db/b1/")
	if err != nil || len(listed) != 3 {
		t.Fatalf("List = %v, %v; want three objects across two pages", listed, err)
	}
	n, err := s.DeletePrefix(ctx, "ns/clickhouse/db/b1/")
	if err != nil || n != 3 {
		t.Fatalf("DeletePrefix = %d, %v", n, err)
	}
	if _, ok := store.objects["ns/clickhouse/db/b2/a"]; !ok || len(store.objects) != 1 {
		t.Fatalf("objects left = %v; want only b2's", store.objects)
	}
	if _, err := s.DeletePrefix(ctx, "/"); err == nil {
		t.Fatal("DeletePrefix of the whole bucket must be refused")
	}
}

func TestS3RefusalIsInTheObjectStoresWords(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{}, pageMax: 10}
	server := httptest.NewServer(store)
	defer server.Close()
	s := &S3{Bucket: Bucket{Endpoint: server.URL, Name: "backups", Region: "auto", AccessKeyID: "wrong", SecretAccessKey: "secret"}}
	_, err := s.List(context.Background(), "x/")
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v; want the store's AccessDenied", err)
	}
}

type fakeClickHouse struct {
	t        *testing.T
	queries  []string
	failWith string
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-ClickHouse-User") != "backup" || r.Header.Get("X-ClickHouse-Key") != "pw" {
		http.Error(w, "Code: 516. DB::Exception: backup: Authentication failed", http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" {
		f.t.Errorf("the query must be the body, not the URL: %q", r.URL.RawQuery)
	}
	body, _ := io.ReadAll(r.Body)
	query := string(body)
	f.queries = append(f.queries, query)
	switch {
	case strings.HasPrefix(query, "BACKUP"):
		if f.failWith != "" {
			http.Error(w, f.failWith, http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "e7a1\tBACKUP_CREATED\n")
	case strings.HasPrefix(query, "SELECT"):
		fmt.Fprint(w, `{"id":"e7a1","status":"BACKUP_CREATED","total_size":"4096"}`+"\n")
	}
}

func clickHouseStore(t *testing.T, server *httptest.Server) Datastore {
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	return Datastore{Host: host, Port: port, Username: "backup", Password: "pw"}
}

func TestClickHouseBacksUpEveryUserDatabaseToTheTarget(t *testing.T) {
	fake := &fakeClickHouse{t: t}
	server := httptest.NewServer(fake)
	defer server.Close()
	target := Target{Bucket: Bucket{Endpoint: "https://acct.r2.cloudflarestorage.com", Name: "backups", AccessKeyID: "key-id", SecretAccessKey: "s'cret"}, Prefix: "ws-1/clickhouse/events/b1/"}
	result, err := ClickHouse{}.Backup(context.Background(), clickHouseStore(t, server), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.SizeBytes != 4096 {
		t.Fatalf("size = %d, want 4096 from system.backups", result.SizeBytes)
	}
	want := "BACKUP ALL EXCEPT DATABASES `system`, `information_schema`, `INFORMATION_SCHEMA` TO S3(" +
		"'https://acct.r2.cloudflarestorage.com/backups/ws-1/clickhouse/events/b1/', 'key-id', 's\\'cret')"
	if fake.queries[0] != want {
		t.Fatalf("query\n got %s\nwant %s", fake.queries[0], want)
	}
	if !strings.Contains(fake.queries[1], "system.backups WHERE id = 'e7a1'") {
		t.Fatalf("size query = %s", fake.queries[1])
	}

	store := clickHouseStore(t, server)
	store.Database = "events"
	if _, err := (ClickHouse{}).Backup(context.Background(), store, target); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fake.queries[2], "BACKUP DATABASE `events` TO S3(") {
		t.Fatalf("one-database query = %s", fake.queries[2])
	}
}

func TestClickHouseRefusalIsReportedWithoutTheSecret(t *testing.T) {
	fake := &fakeClickHouse{t: t, failWith: "Code: 499. DB::Exception: S3 refused 'topsecret': Access Denied"}
	server := httptest.NewServer(fake)
	defer server.Close()
	target := Target{Bucket: Bucket{Endpoint: "https://e", Name: "b", AccessKeyID: "k", SecretAccessKey: "topsecret"}, Prefix: "p/"}
	_, err := ClickHouse{}.Backup(context.Background(), clickHouseStore(t, server), target)
	if err == nil || !strings.Contains(err.Error(), "Access Denied") || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v; want the refusal, redacted", err)
	}
}

func TestMainReportsThroughTheTerminationMessage(t *testing.T) {
	fake := &fakeClickHouse{t: t}
	server := httptest.NewServer(fake)
	defer server.Close()
	store := clickHouseStore(t, server)
	env := map[string]string{
		EnvDatastoreHost: store.Host, EnvDatastorePort: store.Port, EnvDatastoreUsername: "backup", EnvDatastorePassword: "pw",
		EnvBucketEndpoint: "https://acct.r2.cloudflarestorage.com", EnvBucketName: "backups", EnvBucketAccessKey: "k", EnvBucketSecretKey: "s",
	}
	termination := filepath.Join(t.TempDir(), "termination-log")
	var stderr bytes.Buffer
	code := Main([]string{"--datastore", "ClickHouse", "--backup", "b1", "--prefix", "ws/clickhouse/events/b1"}, func(k string) string { return env[k] }, termination, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, _ := os.ReadFile(termination)
	report, ok := ParseReport(string(data))
	if !ok || !report.Succeeded || report.SizeBytes != 4096 {
		t.Fatalf("report = %+v (%s)", report, data)
	}
	if !strings.Contains(fake.queries[0], "/backups/ws/clickhouse/events/b1/'") {
		t.Fatalf("prefix not used: %s", fake.queries[0])
	}

	code = Main([]string{"--datastore", "Postgres", "--backup", "b2", "--prefix", "p"}, func(k string) string { return env[k] }, termination, &stderr)
	data, _ = os.ReadFile(termination)
	report, ok = ParseReport(string(data))
	if code != 1 || !ok || report.Succeeded || !strings.Contains(report.Message, `no backup mechanism for datastore kind "Postgres"`) {
		t.Fatalf("exit %d, report %+v", code, report)
	}
}

func TestReportFitsTheTerminationMessage(t *testing.T) {
	data := Report{Message: strings.Repeat("x", 10000)}.encode()
	if len(data) > 4096 {
		t.Fatalf("report is %d bytes; the kubelet keeps 4096", len(data))
	}
	if _, ok := ParseReport(string(data)); !ok {
		t.Fatal("a truncated report must still parse")
	}
}
