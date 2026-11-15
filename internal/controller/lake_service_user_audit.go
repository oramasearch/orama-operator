/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Unmanaged users (operator#1172). Before operator#770 and operator#771 a Valkey tenant's `app`
// held every command (ACL SETUSER among them) and a Kafka tenant's user ran on a cluster without
// authorization (so it could add SCRAM credentials for any name). Either could have made users of
// its own, which outlive the narrowing of its credential. Once the tenant's connection is
// delivered, the operator signs in with an identity of its own that can only list users, lists
// them once, records the ones it did not make on that identity's Secret, and reports them: in the
// Ready status message and as a Warning event on the Lake. It never drops one: a tenant may rely
// on a user it made, so that is left to the tenant or to Steve.
//
// The identity is least privilege and the same on every service of a kind, so on every cluster:
//   - Valkey: ACL user `orama_operator`, allowed only `ACL USERS`.
//   - Kafka: KafkaUser `<name>-operator`, SCRAM-SHA-512, with only Describe on the cluster
//     resource, which DescribeUserScramCredentials needs. Not a super user.
//
// Its password is generated into Secret `<name>-operator-credentials` (key password), controlled by the Lake
// and deleted last with the service's other password Secrets.

const (
	// serviceOperatorSecretSuffix names the Secret of the operator's own identity in a service. Not
	// <name>-operator: Strimzi's User Operator writes a Secret named after each KafkaUser.
	serviceOperatorSecretSuffix = "-operator-credentials"
	serviceOperatorSecretKey    = "password"
	// serviceUsersAuditedAnnotation, on that Secret, is when the users were listed (RFC 3339, UTC).
	// With it the listing is never repeated and a steady service makes no call.
	serviceUsersAuditedAnnotation = "orama.cloud/users-audited"
	// serviceAuditedUsersAnnotation lists, comma-separated, the users found that the operator
	// did not make. TiDB and Cassandra record theirs under the same annotation (operator#1173,
	// serviceUnmanagedUsersAnnotation there); the two constants become one when both are merged.
	serviceAuditedUsersAnnotation = "orama.cloud/unmanaged-users"
)

// catalogUserAudit is a kind whose users the operator lists once with an identity of its own.
type catalogUserAudit interface {
	title() string
	// operatorSecret names the Secret holding the operator identity's generated password.
	operatorSecret(name string) (secretName, key string)
	// unmanagedUsers lists, sorted, the users the operator did not make.
	unmanagedUsers(ctx context.Context, r *LakeReconciler, in catalogService, found map[string]*unstructured.Unstructured) ([]string, error)
}

// auditedUsersNote is what a Ready service's status message adds for the recorded users.
func auditedUsersNote(recorded string) string {
	if recorded == "" {
		return ""
	}
	return "; users and roles the operator did not make: " + boundedReason(strings.ReplaceAll(recorded, ",", ", "))
}

// reportUnmanagedUsers lists a service's unmanaged users once and returns the note for its status
// message. A failure is logged and tried again on the next pass; it never holds back the tenant's
// connection.
func (r *LakeReconciler) reportUnmanagedUsers(ctx context.Context, in catalogService, found map[string]*unstructured.Unstructured, audit catalogUserAudit) string {
	log := logf.FromContext(ctx).WithValues("namespace", in.namespace, "service", in.svc.Slug, "kind", in.svc.Kind)
	secretName, _ := audit.operatorSecret(in.name)
	secret, err := readServiceSecret(ctx, r.Client, in.namespace, secretName)
	if err != nil || secret == nil {
		return ""
	}
	if secret.Annotations[serviceUsersAuditedAnnotation] != "" {
		return auditedUsersNote(secret.Annotations[serviceAuditedUsersAnnotation])
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	others, err := audit.unmanagedUsers(callCtx, r, in, found)
	if err != nil {
		log.Error(err, "the service's users are not listed yet; tried again on the next pass")
		return ""
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[serviceUsersAuditedAnnotation] = time.Now().UTC().Format(time.RFC3339)
	secret.Annotations[serviceAuditedUsersAnnotation] = strings.Join(others, ",")
	if err := r.Update(ctx, secret); err != nil {
		log.Error(err, "the service's users are listed; the record is written on the next pass")
		return ""
	}
	if len(others) > 0 {
		log.Info("the service holds users the operator did not make; left in place", "users", others)
		if r.Recorder != nil {
			r.Recorder.Eventf(in.lake, corev1.EventTypeWarning, "UnmanagedUsers",
				"%s service %s: users the operator did not make, left in place: %s",
				audit.title(), in.svc.Slug, boundedReason(strings.Join(others, ", ")))
		}
	}
	return auditedUsersNote(secret.Annotations[serviceAuditedUsersAnnotation])
}

// serviceUserLister signs in to a store with the operator's identity and lists its users. Tests
// replace serviceUserListers.
type serviceUserLister interface {
	valkeyACLUsers(ctx context.Context, in catalogService, user, password string) ([]string, error)
	kafkaSCRAMUsers(ctx context.Context, in catalogService, user, password, caPEM string) ([]string, error)
}

var serviceUserListers serviceUserLister = inClusterUserListers{}

type inClusterUserListers struct{}

// --- Valkey ---

// valkeyOperatorUser is the operator's ACL user in every Valkey service: it may run ACL USERS and
// nothing else (AUTH is always allowed).
const valkeyOperatorUser = "orama_operator"

// valkeyKnownUsers are the users the operators make: Valkey's own `default` (switched off), the
// tenant's `app`, the Orama operator's, and valkey-operator 0.6.0's system users and its disabled
// ACL-revision bookkeeping user (internal/controller/users.go).
var valkeyKnownUsers = map[string]bool{
	"default": true, valkeyUser: true, valkeyOperatorUser: true,
	"_operator": true, "_exporter": true, "_replication": true, "_operator_acl_revision": true,
}

// valkeyUnmanagedUsers names, sorted and once each, the users of any node the operators did not
// make.
func valkeyUnmanagedUsers(perNode [][]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, users := range perNode {
		for _, user := range users {
			if valkeyKnownUsers[user] || seen[user] {
				continue
			}
			seen[user] = true
			out = append(out, user)
		}
	}
	sort.Strings(out)
	return out
}

// valkeyACLUsers runs ACL USERS on every node: ACLs are per node in cluster mode, and a user made
// with ACL SETUSER lives on the node it was made on until valkey-operator's next ACL LOAD or the
// node's restart. The client Service is headless, so its name resolves to every node.
func (inClusterUserListers) valkeyACLUsers(ctx context.Context, in catalogService, user, password string) ([]string, error) {
	addresses, err := net.DefaultResolver.LookupHost(ctx, valkeyHost(in))
	if err != nil {
		return nil, fmt.Errorf("resolve the Valkey nodes: %w", err)
	}
	sort.Strings(addresses)
	var perNode [][]string
	for _, address := range addresses {
		users, err := valkeyNodeUsers(ctx, net.JoinHostPort(address, strconv.Itoa(valkeyPort)), user, password)
		if err != nil {
			return nil, err
		}
		perNode = append(perNode, users)
	}
	return valkeyUnmanagedUsers(perNode), nil
}

// valkeyNodeUsers signs in to one node over RESP and returns ACL USERS. The protocol is two
// commands, so no client library is needed.
func valkeyNodeUsers(ctx context.Context, address, user, password string) ([]string, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to Valkey node %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(15 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = conn.SetDeadline(deadline)
	reader := bufio.NewReader(conn)
	if _, err := conn.Write(respCommand("AUTH", user, password)); err != nil {
		return nil, err
	}
	if _, err := readRESP(reader); err != nil {
		return nil, fmt.Errorf("sign in to Valkey node %s: %w", address, err)
	}
	if _, err := conn.Write(respCommand("ACL", "USERS")); err != nil {
		return nil, err
	}
	reply, err := readRESP(reader)
	if err != nil {
		return nil, fmt.Errorf("run ACL USERS on Valkey node %s: %w", address, err)
	}
	users, ok := reply.([]string)
	if !ok {
		return nil, fmt.Errorf("ACL USERS on Valkey node %s answered %T", address, reply)
	}
	return users, nil
}

// respCommand encodes a command as a RESP array of bulk strings.
func respCommand(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return []byte(b.String())
}

// respLimit bounds a reply's sizes, so a node cannot make the operator allocate without bound.
const respLimit = 1 << 20

// readRESP reads one reply: a simple string, an error, an integer, a bulk string, or an array of
// bulk strings (all that AUTH and ACL USERS answer). An error reply is returned as an error.
func readRESP(r *bufio.Reader) (interface{}, error) {
	line, err := readRESPLine(r)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, errors.New(line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		return readRESPBulk(r, line)
	case '*':
		count, err := strconv.Atoi(line[1:])
		if err != nil || count < 0 || count > respLimit {
			return nil, fmt.Errorf("bad array length %q", line)
		}
		items := make([]string, 0, count)
		for i := 0; i < count; i++ {
			header, err := readRESPLine(r)
			if err != nil {
				return nil, err
			}
			if header == "" || header[0] != '$' {
				return nil, fmt.Errorf("array item %q is not a bulk string", header)
			}
			item, err := readRESPBulk(r, header)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	return nil, fmt.Errorf("unknown reply %q", line)
}

func readRESPLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > respLimit {
		return "", errors.New("reply line too long")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func readRESPBulk(r *bufio.Reader, header string) (string, error) {
	size, err := strconv.Atoi(header[1:])
	if err != nil || size < -1 || size > respLimit {
		return "", fmt.Errorf("bad bulk length %q", header)
	}
	if size == -1 {
		return "", nil
	}
	body := make([]byte, size+2)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", err
	}
	return string(body[:size]), nil
}

// --- Kafka ---

// kafkaUnmanagedUsers names, sorted, the SCRAM users that are not a KafkaUser the operator made.
func kafkaUnmanagedUsers(scramUsers []string, managed map[string]bool) []string {
	var out []string
	for _, user := range scramUsers {
		if !managed[user] {
			out = append(out, user)
		}
	}
	sort.Strings(out)
	return out
}

// kafkaSCRAMUsers signs in to the client listener (TLS to the cluster CA, SCRAM-SHA-512) as the
// operator's KafkaUser and lists every SCRAM user (DescribeUserScramCredentials, which needs
// Describe on the cluster). Workspace NetworkPolicy allow-orama-operator-kafka admits the
// operator's pods to the brokers.
func (inClusterUserListers) kafkaSCRAMUsers(ctx context.Context, in catalogService, user, password, caPEM string) ([]string, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("the Kafka cluster CA is not a PEM certificate")
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaBootstrap(in)),
		kgo.DialTLSConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
		kgo.SASL(scram.Auth{User: user, Pass: password}.AsSha512Mechanism()),
		kgo.DialTimeout(10*time.Second),
	)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	described, err := kadm.NewClient(client).DescribeUserSCRAMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe the Kafka's SCRAM users: %w", err)
	}
	if err := described.Error(); err != nil {
		return nil, fmt.Errorf("describe the Kafka's SCRAM users: %w", err)
	}
	users := make([]string, 0, len(described))
	for name := range described {
		users = append(users, name)
	}
	return users, nil
}
