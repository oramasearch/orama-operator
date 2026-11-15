/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Administrator rotation (operator#1172). Before operator#771 a TiDB or Cassandra tenant received
// the administrator's credential, so it knows that password and could still sign in with it. Once
// the tenant's own user is in place and delivered, the operator changes the administrator's
// password once per service, records that on the tenant Secret, and lists the users and roles that
// are neither the administrator's nor the tenant user's. It reports them (the service's status
// message and an event on the Lake) and never drops them: a tenant may rely on a user it made.
//
// Every service is rotated once, including one made after operator#771, whose administrator no
// tenant has seen: it costs one statement and needs no record of which services were exposed.
//
// The new password is written to the tenant Secret (key admin-next) before the store is changed,
// so a pass that stops halfway can finish: the next pass signs in with the old password, or, if
// that no longer works, proves the new one does, then writes it to the administrator's Secret.

const (
	// serviceAdminNextKey holds the administrator password being rotated to, on the tenant Secret,
	// until it is in the store and in the administrator's Secret.
	serviceAdminNextKey = "admin-next"
	// serviceAdminRotatedAnnotation, on the tenant Secret, is when the administrator's password was
	// rotated (RFC 3339, UTC). Its presence means the rotation is done and is never repeated.
	serviceAdminRotatedAnnotation = "orama.cloud/admin-password-rotated"
	// serviceUnmanagedUsersAnnotation, on the tenant Secret, lists (comma-separated) the users or
	// roles found at rotation that the operator did not make.
	serviceUnmanagedUsersAnnotation = "orama.cloud/unmanaged-users"
)

// catalogAdminRotation is implemented by a catalogTenantUser kind whose administrator password the
// operator rotates once the tenant user is delivered.
type catalogAdminRotation interface {
	title() string
	// adminSecret names the Secret holding the administrator's password, the password's key, and
	// the user name's key ("" when the user is always adminUser).
	adminSecret(in catalogService) (secretName, passwordKey, userKey string)
	adminUser() string
	rotateAdmin(ctx context.Context, in catalogService, user, current, next string) error
	otherUsers(ctx context.Context, in catalogService, user, password string) ([]string, error)
}

// unmanagedUsersNote is what a Ready service's status message adds for the users recorded at
// rotation, "" when there were none.
func unmanagedUsersNote(recorded string) string {
	if recorded == "" {
		return ""
	}
	return "; users and roles the operator did not make: " + boundedReason(strings.ReplaceAll(recorded, ",", ", "))
}

// rotateServiceAdministrator rotates the administrator's password of a service whose tenant user
// is delivered, once, and returns the note for its status message. A failure is logged and tried
// again on the next pass; it never holds back the tenant's connection. A rotated service makes no
// call to its store.
func (r *LakeReconciler) rotateServiceAdministrator(ctx context.Context, in catalogService, tenant catalogTenantUser, rotation catalogAdminRotation) string {
	log := logf.FromContext(ctx).WithValues("namespace", in.namespace, "service", in.svc.Slug, "kind", in.svc.Kind)
	tenantName, _ := tenant.tenantSecret(in.name)
	tenantSecret, err := readServiceSecret(ctx, r.Client, in.namespace, tenantName)
	if err != nil || tenantSecret == nil {
		return ""
	}
	if tenantSecret.Annotations[serviceAdminRotatedAnnotation] != "" {
		return unmanagedUsersNote(tenantSecret.Annotations[serviceUnmanagedUsersAnnotation])
	}
	adminName, passwordKey, userKey := rotation.adminSecret(in)
	admin, err := readServiceSecret(ctx, r.Client, in.namespace, adminName)
	if err != nil || admin == nil {
		return ""
	}
	current := string(admin.Data[passwordKey])
	user := rotation.adminUser()
	if userKey != "" {
		user = string(admin.Data[userKey])
	}
	if current == "" || user == "" {
		return ""
	}

	next := string(tenantSecret.Data[serviceAdminNextKey])
	if next == "" {
		if next, err = randHex(servicePasswordBytes); err != nil {
			log.Error(err, "administrator rotation deferred")
			return ""
		}
		tenantSecret.Data[serviceAdminNextKey] = []byte(next)
		if err := r.Update(ctx, tenantSecret); err != nil {
			log.Error(err, "administrator rotation deferred: cannot record the new password")
			return ""
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if current != next {
		if err := rotation.rotateAdmin(callCtx, in, user, current, next); err != nil {
			log.Error(err, "the administrator's password is not rotated yet; tried again on the next pass")
			return ""
		}
		admin.Data[passwordKey] = []byte(next)
		if err := r.Update(ctx, admin); err != nil {
			log.Error(err, "administrator rotated; its Secret is written on the next pass")
			return ""
		}
	}
	others, err := rotation.otherUsers(callCtx, in, user, next)
	if err != nil {
		log.Error(err, "administrator rotated; the other users are listed on the next pass")
		return ""
	}

	delete(tenantSecret.Data, serviceAdminNextKey)
	if tenantSecret.Annotations == nil {
		tenantSecret.Annotations = map[string]string{}
	}
	tenantSecret.Annotations[serviceAdminRotatedAnnotation] = time.Now().UTC().Format(time.RFC3339)
	tenantSecret.Annotations[serviceUnmanagedUsersAnnotation] = strings.Join(others, ",")
	if err := r.Update(ctx, tenantSecret); err != nil {
		log.Error(err, "administrator rotated; the record is written on the next pass")
		return ""
	}
	log.Info("rotated the administrator's password", "unmanagedUsers", others)
	if r.Recorder != nil {
		r.Recorder.Eventf(in.lake, corev1.EventTypeNormal, "AdminPasswordRotated",
			"%s service %s: the administrator's password was rotated", rotation.title(), in.svc.Slug)
		if len(others) > 0 {
			r.Recorder.Eventf(in.lake, corev1.EventTypeWarning, "UnmanagedUsers",
				"%s service %s: users and roles the operator did not make, left in place: %s",
				rotation.title(), in.svc.Slug, boundedReason(strings.Join(others, ", ")))
		}
	}
	return unmanagedUsersNote(tenantSecret.Annotations[serviceUnmanagedUsersAnnotation])
}
