// SPDX-License-Identifier: MIT

package controller

// A lake version is only the image in the Lake (orama-operator#936).
//
// A lake image built from lake#1147 onward carries what the operator needs to run it, and prints it
// with `lake manifest` (lake docs/deployment/operator-interface.md): its embedded schema (applied by
// `lake migrate` from the same image), the Stripe webhook events it handles, its Stripe products,
// and its guard tables. The operator reads the manifest once per image with a Job, keeps the result
// in status.imageManifest, and then:
//
//   - runs the schema migration from the image (PostgresSchema spec.migrate), once per image
//     digest, before the Deployment rolls;
//   - subscribes the Lake's Stripe endpoint to the manifest's events and writes its products;
//   - counts the manifest's guard tables and migrations in restore rehearsals.
//
// An image that predates the command answers `lake manifest` with "unknown argument", its usage
// text and exit status 2, never JSON. For it the operator keeps the vendored path: its copy of
// lake's db/schema.sql applied with psql, and its own Stripe lists.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The manifest pod is found and its log read through these (the manager's ClusterRole has them).
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get

const (
	// lakeManifestImageAnnotation records, on the manifest Job, the spec.image it reads.
	lakeManifestImageAnnotation = "orama.cloud/lake-image"
	// lakeManifestContainer is the manifest Job's only container.
	lakeManifestContainer = "manifest"
	// lakeManifestPoll is how often a Lake waiting for its manifest is looked at again; the
	// owned Job's completion also requeues it.
	lakeManifestPoll = 10 * time.Second
	// lakeManifestRetry is how long a manifest Job that gave no usable answer stays before it is
	// replaced and tried again.
	lakeManifestRetry = 2 * time.Minute
	// lakeManifestLogLimit bounds the pod log read: today's manifest is about 25 KB.
	lakeManifestLogLimit = int64(1 << 20)
	// lakeMigrateWaitSeconds is how long `lake migrate` waits for the control database.
	lakeMigrateWaitSeconds = "600"
	// lakeManifestUsageExit is the exit status of an image that does not know `lake manifest`.
	lakeManifestUsageExit = 2
)

// lakeMigrateFailOnExitCodes are `lake migrate`'s exit codes that a retry cannot fix: 1, a
// statement failed and the transaction rolled back; 2, bad arguments or no database URI. Exit 3
// (the database could not be reached) is retried by the Job's backoff.
var lakeMigrateFailOnExitCodes = []int32{1, 2}

var (
	lakeManifestMigrationPattern = regexp.MustCompile(`^[0-9]{4}_[A-Za-z0-9_]+$`)
	lakeManifestGuardPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*_guard$`)
	lakeManifestArgPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	lakeManifestFlagPattern      = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
	lakeManifestEnvPattern       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	lakeManifestEventPattern     = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)
	lakeManifestProductPattern   = regexp.MustCompile(`^[a-z0-9_]{3,64}$`)
	lakeManifestImageRefPattern  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
)

// lakeManifestDocument is the part of `lake manifest`'s JSON (manifest_version 1) the operator reads.
type lakeManifestDocument struct {
	ManifestVersion int    `json:"manifest_version"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	Revision        string `json:"revision"`
	Schema          struct {
		Version         string   `json:"version"`
		LatestMigration string   `json:"latest_migration"`
		GuardTable      string   `json:"guard_table"`
		GuardTables     []string `json:"guard_tables"`
		Migrations      []string `json:"migrations"`
	} `json:"schema"`
	Migrate struct {
		Args        []string `json:"args"`
		WaitFlag    string   `json:"wait_flag"`
		DatabaseEnv string   `json:"database_env"`
	} `json:"migrate"`
	Stripe struct {
		WebhookEvents []string `json:"webhook_events"`
		Products      []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"products"`
	} `json:"stripe"`
}

// lakeManifestPod is what the manifest Job's pod reported.
type lakeManifestPod struct {
	ExitCode int32
	ImageID  string
	Log      string
}

// lakeImageManifestState is what a reconcile knows about the image's manifest.
type lakeImageManifestState struct {
	// Status is what status.imageManifest holds after this reconcile: the manifest of spec.image
	// once read, or, while it is being read, the previous image's. Nil means the vendored path.
	Status *oramav1alpha1.LakeImageManifest
	// Current is true when Status describes spec.image (or no pod reader is configured, so the
	// vendored path applies): the schema may be reconciled for it.
	Current bool
	// Readiness is Ready, or why the new image is held.
	Readiness lakeComponentReadiness
	// Requeue is when to look again while the manifest is being read.
	Requeue time.Duration
}

// lakeManifestJobName names the Lake's manifest Job.
func lakeManifestJobName(lake *oramav1alpha1.Lake) string { return "lake-" + lake.Name + "-manifest" }

// lakeManifestReaderConfigured reports whether this reconciler can read a pod's log. Without one
// (a reconciler built with neither LakeManifestLogs nor Clientset, as in unit tests that do not
// exercise this) no manifest Job is made and the vendored path applies. cmd/main.go always sets
// Clientset.
func (r *LakeReconciler) lakeManifestReaderConfigured() bool {
	return r.LakeManifestLogs != nil || r.Clientset != nil
}

// reconcileLakeImageManifest reads spec.image's manifest once and returns what the reconcile acts
// on. The result is cached in status.imageManifest by the caller's status write; a cached manifest
// for the same image is used without running anything.
func (r *LakeReconciler) reconcileLakeImageManifest(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (lakeImageManifestState, error) {
	ready := lakeComponentReadiness{phase: "Ready"}
	cached := lake.Status.ImageManifest
	if cached != nil && cached.Image == lake.Spec.Image {
		return lakeImageManifestState{Status: cached, Current: true, Readiness: ready}, nil
	}
	if !r.lakeManifestReaderConfigured() {
		return lakeImageManifestState{Current: true, Readiness: ready}, nil
	}
	pending := lakeImageManifestState{
		Status:    cached,
		Readiness: lakeComponentReadiness{"Progressing", "ReadingImageManifest", "reading `lake manifest` from image " + lake.Spec.Image},
		Requeue:   lakeManifestPoll,
	}
	job, err := r.ensureLakeManifestJob(ctx, lake, ns)
	if err != nil || job == nil {
		return pending, err
	}
	pod, found, err := r.readLakeManifestPod(ctx, job)
	if err != nil {
		pending.Readiness.message += ": " + err.Error()
		return pending, nil
	}
	finished := job.Status.Succeeded > 0 || jobFailed(job)
	if found {
		manifest, err := parseLakeManifest(pod.Log, pod.ExitCode)
		if err == nil {
			manifest.Image = lake.Spec.Image
			manifest.ImageRef = lakeImageRefFromID(pod.ImageID)
			return lakeImageManifestState{Status: manifest, Current: true, Readiness: ready}, nil
		}
		pending.Readiness = lakeComponentReadiness{"Degraded", "ImageManifestInvalid",
			fmt.Sprintf("image %s: %v; the image is held until its manifest can be read", lake.Spec.Image, err)}
		finished = true
	} else if jobFailed(job) {
		words := failedPodWords(ctx, r.reader(), ns, job.Name)
		if words == "" {
			words = jobFailureMessage(job)
		}
		if words == "" {
			words = "the Job failed"
		}
		pending.Readiness = lakeComponentReadiness{"Degraded", "ImageManifestFailed",
			fmt.Sprintf("`lake manifest` from image %s did not run: %s; the image is held", lake.Spec.Image, words)}
	}
	if finished && lakeManifestJobFinishedBefore(job, time.Now().Add(-lakeManifestRetry)) {
		// Try again: a pull or scheduling failure may have cleared. The next reconcile creates a
		// fresh Job once this one is gone.
		policy := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); client.IgnoreNotFound(err) != nil {
			return pending, err
		}
	}
	return pending, nil
}

// ensureLakeManifestJob returns the Job reading spec.image's manifest, creating it when missing.
// A Job for another image is deleted first; nil is returned while it goes.
func (r *LakeReconciler) ensureLakeManifestJob(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: lakeManifestJobName(lake), Namespace: ns}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		if !metav1.IsControlledBy(job, lake) {
			return nil, fmt.Errorf("manifest Job %s is not controlled by Lake %s", job.Name, lake.Name)
		}
		if !job.DeletionTimestamp.IsZero() {
			return nil, nil
		}
		if job.Annotations[lakeManifestImageAnnotation] != lake.Spec.Image {
			policy := metav1.DeletePropagationBackground
			err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy,
				Preconditions: &metav1.Preconditions{UID: &job.UID}})
			return nil, client.IgnoreNotFound(err)
		}
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = lakeManifestJobLabels(lake)
	job.Annotations = map[string]string{lakeManifestImageAnnotation: lake.Spec.Image}
	job.Spec = lakeManifestJobSpec(lake)
	if err := controllerutil.SetControllerReference(lake, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// lakeManifestJobLabels are the manifest Job's labels. They never match the Lake Service's selector
// (no app.kubernetes.io/name), and carry no sandbox identity: `lake manifest` needs no network.
func lakeManifestJobLabels(lake *oramav1alpha1.Lake) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "orama-operator",
		"orama.cloud/lake-manifest":    lake.Name,
	}
}

// lakeManifestJobSpec runs `lake manifest` from spec.image: no environment, no network, no
// database, no service account token. One attempt: an image that does not know the command exits 2
// at once, and that answer is what is read.
func lakeManifestJobSpec(lake *oramav1alpha1.Lake) batchv1.JobSpec {
	securityContext := hardenedSecurityContext()
	securityContext.RunAsNonRoot = boolPtr(true)
	deadline := int64(600) // covers pulling the image
	ttl := int32(3600)
	spec := batchv1.JobSpec{
		BackoffLimit:            int32Ptr(0),
		ActiveDeadlineSeconds:   &deadline,
		TTLSecondsAfterFinished: &ttl,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: lakeManifestJobLabels(lake)},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: boolPtr(false),
				EnableServiceLinks:           boolPtr(false),
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: boolPtr(true),
					RunAsUser:    ptrInt64(lakeRunAsUser),
					RunAsGroup:   ptrInt64(lakeRunAsGroup),
				},
				Containers: []corev1.Container{{
					Name:            lakeManifestContainer,
					Image:           lake.Spec.Image,
					Args:            []string{"manifest"}, // the image's entrypoint is lake
					Resources:       *resourceReqs("32Mi", "10m", "256Mi"),
					SecurityContext: securityContext,
				}},
			},
		},
	}
	if lake.Spec.PullSecretName != "" {
		spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
	}
	return spec
}

// readLakeManifestPod returns the newest pod of this Job (by owner UID: a pod of a replaced Job of
// the same name may still exist) that has terminated: its exit code, the image it ran by digest,
// and its log.
func (r *LakeReconciler) readLakeManifestPod(ctx context.Context, job *batchv1.Job) (lakeManifestPod, bool, error) {
	ns := job.Namespace
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return lakeManifestPod{}, false, err
	}
	pods.Items = slices.DeleteFunc(pods.Items, func(pod corev1.Pod) bool { return !metav1.IsControlledBy(&pod, job) })
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name != lakeManifestContainer || container.State.Terminated == nil {
				continue
			}
			log, err := r.lakeManifestLog(ctx, ns, pod.Name)
			if err != nil {
				return lakeManifestPod{}, false, fmt.Errorf("read manifest pod log: %w", err)
			}
			return lakeManifestPod{ExitCode: container.State.Terminated.ExitCode, ImageID: container.ImageID, Log: log}, true, nil
		}
	}
	return lakeManifestPod{}, false, nil
}

func (r *LakeReconciler) lakeManifestLog(ctx context.Context, ns, podName string) (string, error) {
	if r.LakeManifestLogs != nil {
		return r.LakeManifestLogs(ctx, ns, podName)
	}
	limit := lakeManifestLogLimit
	raw, err := r.Clientset.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{
		Container: lakeManifestContainer, LimitBytes: &limit,
	}).DoRaw(ctx)
	return string(raw), err
}

// lakeManifestJobFinishedBefore reports whether the Job finished (succeeded or failed) before t.
func lakeManifestJobFinishedBefore(job *batchv1.Job, t time.Time) bool {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time.Before(t)
	}
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete) && condition.Status == corev1.ConditionTrue {
			return condition.LastTransitionTime.Time.Before(t)
		}
	}
	return false
}

// lakeImageRefFromID turns a container's imageID (docker.io/oramasearch/lake@sha256:…, possibly
// prefixed docker-pullable://) into a pullable reference, or "" when it names no digest.
func lakeImageRefFromID(imageID string) string {
	ref := strings.TrimPrefix(imageID, "docker-pullable://")
	if !lakeManifestImageRefPattern.MatchString(ref) {
		return ""
	}
	return ref
}

// parseLakeManifest reads the manifest pod's log. Exit 0 with a manifest_version 1 document is a
// supported image; exit 2 with no document is an image that predates the command. Anything else
// is an error, and the image is held.
func parseLakeManifest(log string, exitCode int32) (*oramav1alpha1.LakeImageManifest, error) {
	document, found := findLakeManifestDocument(log)
	switch {
	case exitCode == lakeManifestUsageExit && !found:
		return &oramav1alpha1.LakeImageManifest{Supported: false}, nil
	case exitCode != 0:
		return nil, fmt.Errorf("`lake manifest` exited %d: %s", exitCode, lakeLogTail(log))
	case !found:
		return nil, fmt.Errorf("`lake manifest` printed no manifest_version document: %s", lakeLogTail(log))
	}
	return lakeManifestFromDocument(document)
}

// findLakeManifestDocument decodes the first JSON object in the log, starting at a line that
// begins with "{", that carries a manifest_version.
func findLakeManifestDocument(log string) (lakeManifestDocument, bool) {
	for offset := 0; offset < len(log); {
		index := strings.IndexByte(log[offset:], '{')
		if index < 0 {
			break
		}
		start := offset + index
		offset = start + 1
		if start > 0 && log[start-1] != '\n' {
			continue
		}
		var document lakeManifestDocument
		if err := json.NewDecoder(strings.NewReader(log[start:])).Decode(&document); err == nil && document.ManifestVersion > 0 {
			return document, true
		}
	}
	return lakeManifestDocument{}, false
}

// lakeManifestFromDocument checks every value the operator will put in a Job, a SQL query or a
// Stripe call, and returns the status form.
func lakeManifestFromDocument(document lakeManifestDocument) (*oramav1alpha1.LakeImageManifest, error) {
	if document.ManifestVersion != 1 {
		return nil, fmt.Errorf("manifest_version %d: this operator reads version 1", document.ManifestVersion)
	}
	if document.Name != "lake" {
		return nil, fmt.Errorf("manifest name is %q, not lake", document.Name)
	}
	check := func(field string, values []string, pattern *regexp.Regexp) error {
		if len(values) == 0 {
			return fmt.Errorf("manifest %s is empty", field)
		}
		for _, value := range values {
			if !pattern.MatchString(value) {
				return fmt.Errorf("manifest %s has an invalid entry %q", field, value)
			}
		}
		return nil
	}
	if err := check("schema.migrations", document.Schema.Migrations, lakeManifestMigrationPattern); err != nil {
		return nil, err
	}
	if err := check("schema.guard_tables", document.Schema.GuardTables, lakeManifestGuardPattern); err != nil {
		return nil, err
	}
	if err := check("schema.guard_table", []string{document.Schema.GuardTable}, lakeManifestGuardPattern); err != nil {
		return nil, err
	}
	if err := check("migrate.args", document.Migrate.Args, lakeManifestArgPattern); err != nil {
		return nil, err
	}
	if err := check("migrate.wait_flag", []string{document.Migrate.WaitFlag}, lakeManifestFlagPattern); err != nil {
		return nil, err
	}
	if err := check("migrate.database_env", []string{document.Migrate.DatabaseEnv}, lakeManifestEnvPattern); err != nil {
		return nil, err
	}
	if err := check("stripe.webhook_events", document.Stripe.WebhookEvents, lakeManifestEventPattern); err != nil {
		return nil, err
	}
	if len(document.Stripe.Products) == 0 {
		return nil, fmt.Errorf("manifest stripe.products is empty")
	}
	products := make([]oramav1alpha1.LakeStripeProduct, 0, len(document.Stripe.Products))
	for _, product := range document.Stripe.Products {
		if !lakeManifestProductPattern.MatchString(product.ID) || strings.TrimSpace(product.Name) == "" {
			return nil, fmt.Errorf("manifest stripe.products has an invalid product %q", product.ID)
		}
		products = append(products, oramav1alpha1.LakeStripeProduct{ID: product.ID, Name: product.Name})
	}
	events := slices.Clone(document.Stripe.WebhookEvents)
	sort.Strings(events)
	return &oramav1alpha1.LakeImageManifest{
		Supported:           true,
		Version:             document.Version,
		Revision:            document.Revision,
		SchemaVersion:       document.Schema.Version,
		LatestMigration:     document.Schema.LatestMigration,
		GuardTable:          document.Schema.GuardTable,
		GuardTables:         slices.Clone(document.Schema.GuardTables),
		Migrations:          slices.Clone(document.Schema.Migrations),
		MigrateArgs:         append(slices.Clone(document.Migrate.Args), document.Migrate.WaitFlag, lakeMigrateWaitSeconds),
		DatabaseEnv:         document.Migrate.DatabaseEnv,
		StripeWebhookEvents: slices.Compact(events),
		StripeProducts:      products,
	}, nil
}

func lakeLogTail(log string) string {
	words := strings.Join(strings.Fields(log), " ")
	if len(words) > 300 {
		words = "…" + strings.ToValidUTF8(words[len(words)-300:], "")
	}
	if words == "" {
		return "no output"
	}
	return words
}

// lakeManifestSupported reports whether the image migrates itself and publishes its lists.
func lakeManifestSupported(manifest *oramav1alpha1.LakeImageManifest) bool {
	return manifest != nil && manifest.Supported
}

// lakeImageSchemaVersion is the PostgresSchema version of a self-migrating image: its digest, so
// the migration runs once per image digest.
func lakeImageSchemaVersion(lake *oramav1alpha1.Lake, manifest *oramav1alpha1.LakeImageManifest) string {
	if _, digest, ok := strings.Cut(manifest.ImageRef, "@"); ok {
		return "image:" + digest
	}
	return "image:" + schemaVersion(lake.Spec.Image)
}

// lakeDesiredSchemaVersion is the PostgresSchema version the Lake's current image needs, or ""
// while the image's manifest is still being read (then the PostgresSchema's own version counts).
func lakeDesiredSchemaVersion(lake *oramav1alpha1.Lake, manifest *oramav1alpha1.LakeImageManifest) string {
	switch {
	case manifest == nil:
		return lakeSchemaVersion()
	case manifest.Image != lake.Spec.Image:
		return ""
	case manifest.Supported:
		return lakeImageSchemaVersion(lake, manifest)
	default:
		return lakeSchemaVersion()
	}
}

// lakeWebhookEvents are the Stripe events the Lake's endpoint subscribes to: the image's, or the
// operator's list for an image that predates `lake manifest`.
func lakeWebhookEvents(manifest *oramav1alpha1.LakeImageManifest) []string {
	if lakeManifestSupported(manifest) {
		return slices.Clone(manifest.StripeWebhookEvents)
	}
	return slices.Clone(lakeStripeWebhookEvents)
}

// lakeCatalogProducts are the Lake's PaymentCatalog products: the image's, or the operator's list.
func lakeCatalogProducts(manifest *oramav1alpha1.LakeImageManifest) []oramav1alpha1.PaymentProduct {
	if !lakeManifestSupported(manifest) {
		return lakePaymentProducts()
	}
	products := make([]oramav1alpha1.PaymentProduct, 0, len(manifest.StripeProducts))
	for _, product := range manifest.StripeProducts {
		products = append(products, oramav1alpha1.PaymentProduct{ID: product.ID, Name: product.Name})
	}
	return products
}

// lakeVerificationQueries are the restore rehearsal's comparison query and missing-migration
// query: from the Lake's image manifest when it has one, else from the vendored schema.
func lakeVerificationQueries(lake *oramav1alpha1.Lake) (verification, missing string) {
	manifest := lake.Status.ImageManifest
	if !lakeManifestSupported(manifest) {
		return postgresVerificationQuery, postgresMissingMigrationsQuery
	}
	missing = missingMigrationsQuery(manifest.Migrations)
	return verificationQuery(guardCountQuery(manifest.GuardTables), missing), missing
}

// guardCountQuery counts each guard table; a guard missing on either side fails the query.
func guardCountQuery(guards []string) string {
	var query strings.Builder
	query.WriteString("\n")
	for i, guard := range guards {
		if i > 0 {
			query.WriteString("UNION ALL ")
		}
		query.WriteString("SELECT '" + guard + "=' || count(*) FROM " + guard + "\n")
	}
	return query.String()
}
