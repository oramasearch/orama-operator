/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

// Backup runs one backup as a Job from the operator's own image (operator#453, docs/backups.md):
// `/manager backup --datastore <kind> --backup <name> --prefix <path>/<name>`, in the Backup's
// namespace (the datastore's), under the restricted Pod Security level, with no Kubernetes
// credentials, egress only to DNS, its own namespace and public HTTPS, the datastore's connection
// Secret and a copy of the bucket's scoped credentials. The Job reports through its termination
// message; this controller writes that to the Backup's status. Deleting a Backup deletes its
// objects from the bucket.

const (
	backupFinalizer          = "orama.cloud/backup-objects"
	backupScheduleLabel      = "orama.cloud/backup-schedule"
	backupDatastoreLabel     = "orama.cloud/backup-datastore"
	backupNameLabel          = "orama.cloud/backup"
	backupScheduledAtAnnot   = "orama.cloud/scheduled-at"
	backupContainerName      = "backup"
	backupJobTTL             = int32(7 * 24 * 3600)
	backupJobDeadline        = int64(6 * 3600)
	backupWaitPoll           = 30 * time.Second
	backupConditionComplete  = "Complete"
	backupOperatorManagedTag = "orama-operator"
)

// BackupObjectStore deletes a backup's objects; the real one is backup.S3. Injected for tests.
type BackupObjectStore interface {
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// BackupReconciler reconciles Backup.
type BackupReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	APIReader         client.Reader
	OperatorNamespace string
	// Image is the operator's own image; empty means read it from the manager pod.
	Image string
	// ObjectStore opens a bucket; nil means backup.S3.
	ObjectStore func(backup.Bucket) BackupObjectStore
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=orama.cloud,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orama.cloud,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orama.cloud,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=orama.cloud,resources=cloudflarer2buckets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *BackupReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b oramav1alpha1.Backup
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &b)
	}
	if !controllerutil.ContainsFinalizer(&b, backupFinalizer) {
		controllerutil.AddFinalizer(&b, backupFinalizer)
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
	if b.Status.Phase == oramav1alpha1.BackupSucceeded || b.Status.Phase == oramav1alpha1.BackupFailed {
		// Done: the bucket credentials and the network policy were only for the run.
		return ctrl.Result{}, r.cleanupRun(ctx, &b)
	}

	desired := *b.Status.DeepCopy()
	result, err := r.run(ctx, &b, &desired)
	if writeErr := r.writeStatus(ctx, &b, desired); writeErr != nil {
		return ctrl.Result{}, writeErr
	}
	return settle(result, err)
}

// run advances one Backup and fills desired.
func (r *BackupReconciler) run(ctx context.Context, b *oramav1alpha1.Backup, desired *oramav1alpha1.BackupStatus) (ctrl.Result, error) {
	kind := string(b.Spec.Datastore.Kind)
	if !backup.Supported(kind) {
		r.finish(b, desired, false, "MechanismNotAvailable", fmt.Sprintf("no backup mechanism for datastore kind %s yet; supported: %s (operator#453)", kind, strings.Join(backup.SupportedKinds(), ", ")), 0)
		return ctrl.Result{}, nil
	}
	prefix := backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path) + "/" + b.Name

	job := &batchv1.Job{}
	jobName := backupJobName(b.Name)
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: jobName}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if apierrors.IsNotFound(err) {
		if desired.Phase == oramav1alpha1.BackupRunning {
			r.finish(b, desired, false, "JobLost", fmt.Sprintf("Job %s was deleted before it reported", jobName), 0)
			return ctrl.Result{}, nil
		}
		bucket, reason, message, err := r.resolveBucket(ctx, b)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reason != "" {
			r.wait(b, desired, reason, message)
			return ctrl.Result{RequeueAfter: backupWaitPoll}, nil
		}
		image, err := r.operatorImage(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if image == "" {
			r.wait(b, desired, "OperatorImageUnknown", "the operator's own image could not be read from its pod")
			return ctrl.Result{RequeueAfter: backupWaitPoll}, nil
		}
		if err := r.createRun(ctx, b, bucket, image, prefix, jobName); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.NewTime(r.now())
		desired.Phase, desired.JobName, desired.StartTime = oramav1alpha1.BackupRunning, jobName, &now
		desired.Location = fmt.Sprintf("s3://%s/%s/", string(bucket.Data["bucket"]), prefix)
		desired.Message = fmt.Sprintf("Job %s is running", jobName)
		apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupConditionComplete, Status: metav1.ConditionFalse, Reason: "Running", Message: desired.Message, ObservedGeneration: b.Generation})
		return ctrl.Result{}, nil
	}

	finished, succeeded, jobMessage := jobOutcome(job)
	if !finished {
		return ctrl.Result{}, nil
	}
	report, ok, err := r.report(ctx, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case ok && report.Succeeded && succeeded:
		r.finish(b, desired, true, "BackupSucceeded", report.Message, report.SizeBytes)
	case ok:
		r.finish(b, desired, false, "BackupFailed", report.Message, 0)
	case succeeded:
		r.finish(b, desired, false, "NoReport", fmt.Sprintf("Job %s completed without a report", jobName), 0)
	default:
		r.finish(b, desired, false, "JobFailed", fmt.Sprintf("Job %s failed: %s", jobName, jobMessage), 0)
	}
	return ctrl.Result{}, nil
}

func (r *BackupReconciler) wait(b *oramav1alpha1.Backup, desired *oramav1alpha1.BackupStatus, reason, message string) {
	desired.Phase, desired.Message = oramav1alpha1.BackupPending, message
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupConditionComplete, Status: metav1.ConditionFalse, Reason: reason, Message: message, ObservedGeneration: b.Generation})
}

func (r *BackupReconciler) finish(b *oramav1alpha1.Backup, desired *oramav1alpha1.BackupStatus, succeeded bool, reason, message string, size int64) {
	now := metav1.NewTime(r.now())
	desired.CompletionTime, desired.Message, desired.SizeBytes = &now, message, size
	status := metav1.ConditionFalse
	desired.Phase = oramav1alpha1.BackupFailed
	if succeeded {
		status, desired.Phase = metav1.ConditionTrue, oramav1alpha1.BackupSucceeded
	}
	apimeta.SetStatusCondition(&desired.Conditions, metav1.Condition{Type: backupConditionComplete, Status: status, Reason: reason, Message: message, ObservedGeneration: b.Generation})
}

func (r *BackupReconciler) writeStatus(ctx context.Context, b *oramav1alpha1.Backup, desired oramav1alpha1.BackupStatus) error {
	if apiequality.Semantic.DeepEqual(b.Status, desired) {
		return nil
	}
	b.Status = desired
	return r.Status().Update(ctx, b)
}

// resolveBucket returns the bucket's scoped credentials Secret, or the reason it cannot yet.
func (r *BackupReconciler) resolveBucket(ctx context.Context, b *oramav1alpha1.Backup) (*corev1.Secret, string, string, error) {
	return resolveBackupBucket(ctx, r.Client, b.Namespace, b.Spec.Bucket)
}

func resolveBackupBucket(ctx context.Context, c client.Client, namespace string, ref oramav1alpha1.BackupBucketRef) (*corev1.Secret, string, string, error) {
	bucketNamespace := ref.Namespace
	if bucketNamespace == "" {
		bucketNamespace = namespace
	}
	var bucket oramav1alpha1.CloudflareR2Bucket
	if err := c.Get(ctx, client.ObjectKey{Namespace: bucketNamespace, Name: ref.Name}, &bucket); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "BucketNotFound", fmt.Sprintf("CloudflareR2Bucket %s/%s does not exist", bucketNamespace, ref.Name), nil
		}
		return nil, "", "", err
	}
	if bucket.Spec.Credentials == nil {
		return nil, "BucketHasNoCredentials", fmt.Sprintf("CloudflareR2Bucket %s/%s has no spec.credentials, so there are no credentials scoped to it", bucketNamespace, ref.Name), nil
	}
	if bucketNamespace != namespace && !slices.Contains(bucket.Spec.Credentials.AllowedNamespaces, namespace) {
		return nil, "BucketNotAllowed", fmt.Sprintf("CloudflareR2Bucket %s/%s does not list namespace %s in spec.credentials.allowedNamespaces", bucketNamespace, ref.Name, namespace), nil
	}
	if !bucket.Status.Ready || bucket.Status.CredentialsSecret == "" {
		message := bucket.Status.Message
		if message == "" {
			message = "not reconciled yet"
		}
		return nil, "BucketNotReady", fmt.Sprintf("CloudflareR2Bucket %s/%s is not ready: %s", bucketNamespace, ref.Name, message), nil
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: bucketNamespace, Name: bucket.Status.CredentialsSecret}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "BucketNotReady", fmt.Sprintf("the credentials Secret %s/%s does not exist yet", bucketNamespace, bucket.Status.CredentialsSecret), nil
		}
		return nil, "", "", err
	}
	return &secret, "", "", nil
}

// createRun writes the bucket credentials copy, the network policy and the Job.
func (r *BackupReconciler) createRun(ctx context.Context, b *oramav1alpha1.Backup, bucket *corev1.Secret, image, prefix, jobName string) error {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": backupOperatorManagedTag,
		"app.kubernetes.io/component":  "backup",
		backupNameLabel:                dnsLabelValue(b.Name),
	}
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-bucket", Namespace: b.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, credentials, func() error {
		if credentials.ResourceVersion != "" && !metav1.IsControlledBy(credentials, b) {
			return fmt.Errorf("Secret %s/%s exists and is not this Backup's", b.Namespace, credentials.Name)
		}
		credentials.Labels = labels
		credentials.Type = corev1.SecretTypeOpaque
		credentials.Data = map[string][]byte{}
		for _, key := range []string{"accessKeyId", "secretAccessKey", "endpoint", "bucket", "region"} {
			credentials.Data[key] = append([]byte(nil), bucket.Data[key]...)
		}
		return controllerutil.SetControllerReference(b, credentials, r.Scheme)
	}); err != nil {
		return err
	}

	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns, https := intstr.FromInt32(53), intstr.FromInt32(443)
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: b.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = labels
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{backupNameLabel: dnsLabelValue(b.Name)}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{ // DNS
					To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				},
				{ // the datastore, in this namespace
					To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				},
				{ // the object store: public HTTPS only
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
						CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"},
					}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
				},
			},
		}
		return controllerutil.SetControllerReference(b, policy, r.Scheme)
	}); err != nil {
		return err
	}

	pullSecrets, err := copyOperatorPullSecrets(ctx, r.Client, r.APIReader, r.OperatorNamespace, b.Namespace)
	if err != nil {
		return err
	}
	job := newBackupJob(b, image, prefix, jobName, credentials.Name, pullSecrets, labels)
	if err := controllerutil.SetControllerReference(b, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func newBackupJob(b *oramav1alpha1.Backup, image, prefix, jobName, bucketSecret string, pullSecrets []string, labels map[string]string) *batchv1.Job {
	connection := b.Spec.Datastore.ConnectionSecretRef.Name
	fromSecret := func(env, secret, key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key, Optional: boolPtr(optional),
		}}}
	}
	var references []corev1.LocalObjectReference
	for _, name := range pullSecrets {
		references = append(references, corev1.LocalObjectReference{Name: name})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: b.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   int64Ptr(backupJobDeadline),
			TTLSecondsAfterFinished: int32Ptr(backupJobTTL),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					ImagePullSecrets:             references,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						RunAsUser:      int64Ptr(65532),
						RunAsGroup:     int64Ptr(65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    backupContainerName,
						Image:   image,
						Command: []string{"/manager", "backup"},
						Args: []string{
							"--datastore", string(b.Spec.Datastore.Kind),
							"--backup", b.Name,
							"--prefix", prefix,
							"--timeout", fmt.Sprintf("%ds", backupJobDeadline-60),
						},
						Env: []corev1.EnvVar{
							fromSecret(backup.EnvDatastoreHost, connection, "host", false),
							fromSecret(backup.EnvDatastorePort, connection, "port", true),
							fromSecret(backup.EnvDatastoreUsername, connection, "username", false),
							fromSecret(backup.EnvDatastorePassword, connection, "password", false),
							fromSecret(backup.EnvDatastoreDatabase, connection, "database", true),
							fromSecret(backup.EnvBucketEndpoint, bucketSecret, "endpoint", false),
							fromSecret(backup.EnvBucketName, bucketSecret, "bucket", false),
							fromSecret(backup.EnvBucketRegion, bucketSecret, "region", true),
							fromSecret(backup.EnvBucketAccessKey, bucketSecret, "accessKeyId", false),
							fromSecret(backup.EnvBucketSecretKey, bucketSecret, "secretAccessKey", false),
						},
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							RunAsNonRoot:             boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
}

// jobOutcome reads a Job's terminal condition.
func jobOutcome(job *batchv1.Job) (finished, succeeded bool, message string) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return true, true, condition.Message
		case batchv1.JobFailed:
			message = condition.Reason
			if condition.Message != "" {
				message += ": " + condition.Message
			}
			return true, false, message
		}
	}
	return false, false, ""
}

// report reads the backup container's termination message from the Job's pods.
func (r *BackupReconciler) report(ctx context.Context, job *batchv1.Job) (backup.Report, bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return backup.Report{}, false, err
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name != backupContainerName || container.State.Terminated == nil {
				continue
			}
			if report, ok := backup.ParseReport(container.State.Terminated.Message); ok {
				return report, true, nil
			}
		}
	}
	return backup.Report{}, false, nil
}

func (r *BackupReconciler) cleanupRun(ctx context.Context, b *oramav1alpha1.Backup) error {
	jobName := backupJobName(b.Name)
	for _, object := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: jobName + "-bucket", Namespace: b.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: b.Namespace}},
	} {
		if err := deleteIfExists(ctx, r.Client, object); err != nil {
			return err
		}
	}
	return nil
}

// finalize deletes the backup's objects from the bucket. A bucket resource that is gone, or has no
// credentials, keeps the objects (a Retain bucket outlives everything) and is logged.
func (r *BackupReconciler) finalize(ctx context.Context, b *oramav1alpha1.Backup) error {
	if !controllerutil.ContainsFinalizer(b, backupFinalizer) {
		return nil
	}
	if b.Status.Location != "" {
		secret, reason, message, err := resolveBackupBucket(ctx, r.Client, b.Namespace, b.Spec.Bucket)
		if err != nil {
			return err
		}
		if reason == "BucketNotReady" {
			return fmt.Errorf("deleting Backup %s/%s: %s", b.Namespace, b.Name, message)
		}
		if reason != "" {
			logf.FromContext(ctx).Info("Backup deleted; its objects are kept", "backup", b.Name, "location", b.Status.Location, "reason", message)
		} else {
			bucket, err := backup.BucketFromSecret(secret.Data)
			if err != nil {
				return err
			}
			prefix := backupPrefix(b.Namespace, b.Spec.Datastore, b.Spec.Path) + "/" + b.Name + "/"
			if _, err := r.objectStore(bucket).DeletePrefix(ctx, prefix); err != nil {
				return fmt.Errorf("delete the objects of Backup %s/%s: %w", b.Namespace, b.Name, err)
			}
		}
	}
	controllerutil.RemoveFinalizer(b, backupFinalizer)
	return r.Update(ctx, b)
}

func (r *BackupReconciler) objectStore(bucket backup.Bucket) BackupObjectStore {
	if r.ObjectStore != nil {
		return r.ObjectStore(bucket)
	}
	return &backup.S3{Bucket: bucket}
}

// operatorImage is the operator's own image: the manager container's, read from its pod.
func (r *BackupReconciler) operatorImage(ctx context.Context) (string, error) {
	if r.Image != "" {
		return r.Image, nil
	}
	podName := os.Getenv("HOSTNAME")
	if r.APIReader == nil || r.OperatorNamespace == "" || podName == "" {
		return "", nil
	}
	var pod corev1.Pod
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: podName}, &pod); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "manager" {
			return container.Image, nil
		}
	}
	return "", nil
}

// backupPrefix is the bucket prefix a datastore's backups go under, without a trailing slash.
func backupPrefix(namespace string, store oramav1alpha1.BackupDatastoreRef, path string) string {
	if path != "" {
		return strings.Trim(path, "/")
	}
	return strings.ToLower(fmt.Sprintf("%s/%s/%s", namespace, store.Kind, store.Name))
}

// backupJobName is a DNS label (the Job's name is also a pod label) derived from the Backup's name.
func backupJobName(name string) string {
	const suffix = "-backup"
	label := dnsLabel(name)
	if len(label)+len(suffix) <= 56 { // 63 less the "-bucket" suffix of the credentials copy
		return label + suffix
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return strings.Trim(label[:56-len(suffix)-len(hash)-1], "-") + "-" + hash + suffix
}

// dnsLabelValue is a label value derived from a name (63 characters at most).
func dnsLabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return strings.Trim(name[:54], "-.") + "-" + hash
}

func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&oramav1alpha1.Backup{}).
		Owns(&batchv1.Job{}).
		Named("backup").
		Complete(r)
}
