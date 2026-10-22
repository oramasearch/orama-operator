/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	icImage     = "oramasearch/airbyte-fargate-consumer:dev-0123abc"
	icQueueURL  = "https://sqs.us-east-1.amazonaws.com/007522357987/orama-prod2-import"
	icBucket    = "airbyte-exports-prod"
	icAWSSecret = "v1-import-aws"
	icAPIKey    = "private-api-key-0003"
)

// importConsumerOC is a dashboard reading Secret dash-env with the import consumer enabled.
func importConsumerOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	oc := dashOC(func(d *oramav1alpha1.DashboardSpec) {
		d.EnvFrom = []corev1.EnvFromSource{secretSource("dash-env")}
	})
	oc.Spec.Components.ImportConsumer = oramav1alpha1.ImportConsumerSpec{
		Enabled:                 true,
		Image:                   icImage,
		QueueURL:                icQueueURL,
		Bucket:                  icBucket,
		AWSCredentialsSecretRef: &corev1.LocalObjectReference{Name: icAWSSecret},
	}
	if mut != nil {
		mut(oc)
	}
	return oc
}

func icDashEnv() *corev1.Secret {
	return secretOf("dash-env", map[string]string{"PRIVATE_API_KEY": icAPIKey})
}

func icAWS(secretKey string) *corev1.Secret {
	return secretOf(icAWSSecret, map[string]string{awsAccessKeyIDKey: "AKIAEXAMPLE", awsSecretAccessKeyKey: secretKey})
}

// importConsumer is the stored consumer Deployment, or nil.
func (h *engineHarness) importConsumer() *appsv1.Deployment {
	h.t.Helper()
	dep := &appsv1.Deployment{}
	if !h.get("v1-import-consumer", dep) {
		return nil
	}
	return dep
}

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

// The consumer runs the image's queue worker with the queue, the bucket, the AWS keys from the named
// Secret, the in-cluster dashboard and the dashboard's own PRIVATE_API_KEY, and nothing else.
func TestImportConsumerRendersTheQueueWorker(t *testing.T) {
	oc := importConsumerOC(nil)
	oc.Spec.Images.PullSecretName = "regcred"
	h := newEngineHarness(t, oc, icDashEnv(), icAWS("secret-1"))
	h.mustRun()

	dep := h.importConsumer()
	if dep == nil {
		t.Fatal("no v1-import-consumer Deployment")
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != icImage || !reflect.DeepEqual(c.Args, []string{"run", "-A", "src/sqs-worker.ts"}) {
		t.Fatalf("image %q args %v", c.Image, c.Args)
	}
	if len(c.EnvFrom) != 0 {
		t.Fatalf("envFrom = %v, want none: the consumer must not load the shared ConfigMap or a component Secret", c.EnvFrom)
	}
	if *dep.Spec.Replicas != 1 || *pod.TerminationGracePeriodSeconds != importConsumerGracePeriod {
		t.Fatalf("replicas %d grace %d", *dep.Spec.Replicas, *pod.TerminationGracePeriodSeconds)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("imagePullSecrets = %v", pod.ImagePullSecrets)
	}
	env := envByName(c)
	literal := map[string]string{
		"NODE_ENV":                "production",
		"SQS_QUEUE_URL":           icQueueURL,
		"S3_BUCKET":               icBucket,
		"AWS_REGION":              "us-east-1",
		"DASHBOARD_URL":           "http://v1-dashboard:3000",
		"WORKER_CONCURRENCY":      "2",
		"IMPORT_MAX_OLD_SPACE_MB": "528", // (1536 - 128) * 3/4 / 2
		"PORT":                    "8080",
	}
	for k, v := range literal {
		if env[k].Value != v || env[k].ValueFrom != nil {
			t.Errorf("%s = %+v, want the literal %q", k, env[k], v)
		}
	}
	secretRef := map[string][2]string{
		"AWS_ACCESS_KEY_ID":     {icAWSSecret, "AWS_ACCESS_KEY_ID"},
		"AWS_SECRET_ACCESS_KEY": {icAWSSecret, "AWS_SECRET_ACCESS_KEY"},
		"PRIVATE_API_KEY":       {"dash-env", "PRIVATE_API_KEY"},
	}
	for k, w := range secretRef {
		ref := env[k].ValueFrom
		if env[k].Value != "" || ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != w[0] || ref.SecretKeyRef.Key != w[1] {
			t.Errorf("%s = %+v, want secretKeyRef %s/%s", k, env[k], w[0], w[1])
		}
	}
	if len(env) != len(literal)+len(secretRef) {
		t.Errorf("env has %d variables, want %d: %v", len(env), len(literal)+len(secretRef), slices.Sorted(func(yield func(string) bool) {
			for k := range env {
				if !yield(k) {
					return
				}
			}
		}))
	}
	if dep.Spec.Template.Annotations[credentialsHashKey] == "" {
		t.Error("no credentials hash on the pod template, so a rotated AWS key would not roll the consumer")
	}
	if c.Resources.Limits.Memory().Cmp(resource.MustParse("1536Mi")) != 0 {
		t.Errorf("memory limit = %v, want the 1536Mi default", c.Resources.Limits.Memory())
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Errorf("readiness probe = %+v, want TCP 8080 (the worker's health listener)", c.ReadinessProbe)
	}
}

// A rotated AWS key or PRIVATE_API_KEY changes the pod template, so the consumer rolls.
func TestImportConsumerRollsOnACredentialChange(t *testing.T) {
	h := newEngineHarness(t, importConsumerOC(nil), icDashEnv(), icAWS("secret-1"))
	h.mustRun()
	before := h.importConsumer().Spec.Template.Annotations[credentialsHashKey]

	sec := &corev1.Secret{}
	h.get(icAWSSecret, sec)
	sec.Data[awsSecretAccessKeyKey] = []byte("secret-2")
	if err := h.cl.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	afterAWS := h.importConsumer().Spec.Template.Annotations[credentialsHashKey]
	if afterAWS == before {
		t.Fatal("the credentials hash did not change with the AWS secret key")
	}

	dash := &corev1.Secret{}
	h.get("dash-env", dash)
	dash.Data["PRIVATE_API_KEY"] = []byte("private-api-key-0004")
	if err := h.cl.Update(context.Background(), dash); err != nil {
		t.Fatal(err)
	}
	h.mustRun()
	if h.importConsumer().Spec.Template.Annotations[credentialsHashKey] == afterAWS {
		t.Fatal("the credentials hash did not change with PRIVATE_API_KEY")
	}
}

// Off by default; turning it off removes the Deployment, its Service and its PDB.
func TestImportConsumerOffRunsNothingAndTurningItOffRemovesIt(t *testing.T) {
	off := importConsumerOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.ImportConsumer = oramav1alpha1.ImportConsumerSpec{}
	})
	h := newEngineHarness(t, off, icDashEnv())
	h.mustRun()
	if h.importConsumer() != nil {
		t.Fatal("a consumer Deployment exists while the consumer is off")
	}

	h2 := newEngineHarness(t, importConsumerOC(nil), icDashEnv(), icAWS("secret-1"))
	h2.mustRun()
	if h2.importConsumer() == nil {
		t.Fatal("no consumer Deployment while enabled")
	}
	h2.updateCR(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ImportConsumer.Enabled = false })
	h2.mustRun()
	if h2.importConsumer() != nil {
		t.Fatal("the consumer Deployment survived turning the consumer off")
	}
	if h2.get("v1-import-consumer", &corev1.Service{}) {
		t.Fatal("the consumer Service survived turning the consumer off")
	}
}

// The step's configuration errors name the cause, never a value, and write no Deployment.
func TestImportConsumerConfigurationErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*oramav1alpha1.OramaCloudV1)
		objs []client.Object
		want string
	}{
		{name: "no dashboard", mut: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Dashboard.Enabled = false },
			objs: []client.Object{icDashEnv(), icAWS("s")}, want: "importConsumer requires the dashboard enabled"},
		{name: "no image", mut: func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ImportConsumer.Image = "" },
			objs: []client.Object{icDashEnv(), icAWS("s")}, want: "has no [image]"},
		{name: "no queue, bucket or Secret", mut: func(o *oramav1alpha1.OramaCloudV1) {
			ic := &o.Spec.Components.ImportConsumer
			ic.QueueURL, ic.Bucket, ic.AWSCredentialsSecretRef = "", "", nil
		}, objs: []client.Object{icDashEnv()}, want: "has no [queueURL bucket awsCredentialsSecretRef]"},
		{name: "no AWS Secret", objs: []client.Object{icDashEnv()},
			want: "importConsumer.awsCredentialsSecretRef: Secret orama-system/v1-import-aws does not exist"},
		{name: "an AWS Secret without the secret key",
			objs: []client.Object{icDashEnv(), secretOf(icAWSSecret, map[string]string{awsAccessKeyIDKey: "AKIAEXAMPLE"})},
			want: "has no non-empty key AWS_SECRET_ACCESS_KEY"},
		{name: "no PRIVATE_API_KEY anywhere", objs: []client.Object{secretOf("dash-env", map[string]string{"OTHER": "x"}), icAWS("s")},
			want: "importConsumer needs PRIVATE_API_KEY"},
		{name: "PRIVATE_API_KEY from a ConfigMap", mut: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.EnvFrom = []corev1.EnvFromSource{configMapSource("dash-cm")}
		}, objs: []client.Object{configMapOf("dash-cm", map[string]string{"PRIVATE_API_KEY": icAPIKey}), icAWS("s")},
			want: "it must come from a Secret"},
		{name: "PRIVATE_API_KEY as a literal", mut: func(o *oramav1alpha1.OramaCloudV1) {
			o.Spec.Components.Dashboard.Env = []corev1.EnvVar{{Name: "PRIVATE_API_KEY", Value: icAPIKey}}
		}, objs: []client.Object{icDashEnv(), icAWS("s")}, want: "the import consumer read it from the same Secret as the dashboard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngineHarness(t, append([]client.Object{importConsumerOC(tc.mut)}, tc.objs...)...)
			err := h.mustFail(tc.want)
			if strings.Contains(err.Error(), icAPIKey) || strings.Contains(err.Error(), "AKIAEXAMPLE") {
				t.Fatalf("the error leaks a value: %v", err)
			}
			if h.importConsumer() != nil {
				t.Fatal("a consumer Deployment was written despite the error")
			}
			if c := h.stored().Status.Conditions; len(c) == 0 || !strings.Contains(c[0].Message, "import-consumer") {
				t.Fatalf("Ready condition = %+v, want it to name import-consumer", c)
			}
		})
	}
}

// dashboard.env's secretKeyRef wins over envFrom, as it does for the dashboard itself.
func TestImportConsumerSendsTheKeyTheDashboardChecks(t *testing.T) {
	oc := importConsumerOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Dashboard.Env = []corev1.EnvVar{{Name: "PRIVATE_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "dash-override"}, Key: "key"}}}}
	})
	h := newEngineHarness(t, oc, icDashEnv(), icAWS("s"), secretOf("dash-override", map[string]string{"key": "override"}))
	h.mustRun()
	ref := envByName(h.importConsumer().Spec.Template.Spec.Containers[0])["PRIVATE_API_KEY"].ValueFrom.SecretKeyRef
	if ref.Name != "dash-override" || ref.Key != "key" {
		t.Fatalf("PRIVATE_API_KEY from %s/%s, want dash-override/key", ref.Name, ref.Key)
	}
}

// Concurrency, replicas, the region and resources reach the pod, and the import heap follows them.
func TestImportConsumerSizing(t *testing.T) {
	oc := importConsumerOC(func(o *oramav1alpha1.OramaCloudV1) {
		ic := &o.Spec.Components.ImportConsumer
		ic.Concurrency, ic.Replicas, ic.Region = 4, 3, "eu-west-1"
		ic.Resources = resourceReqs("512Mi", "200m", "4Gi")
	})
	h := newEngineHarness(t, oc, icDashEnv(), icAWS("s"))
	h.mustRun()
	dep := h.importConsumer()
	env := envByName(dep.Spec.Template.Spec.Containers[0])
	if *dep.Spec.Replicas != 3 || env["WORKER_CONCURRENCY"].Value != "4" || env["AWS_REGION"].Value != "eu-west-1" {
		t.Fatalf("replicas %d concurrency %q region %q", *dep.Spec.Replicas, env["WORKER_CONCURRENCY"].Value, env["AWS_REGION"].Value)
	}
	if got := env["IMPORT_MAX_OLD_SPACE_MB"].Value; got != strconv.Itoa((4096-128)*3/4/4) {
		t.Fatalf("IMPORT_MAX_OLD_SPACE_MB = %s", got)
	}
}

func TestImportConsumerHeapStaysUnderTheLimit(t *testing.T) {
	cases := []struct {
		res         *corev1.ResourceRequirements
		concurrency int32
		want        int64
	}{
		{importConsumerResources(), 2, 528},
		{importConsumerResources(), 1, 1056},
		{resourceReqs("64Mi", "10m", "256Mi"), 16, importConsumerMinHeapMi},
		{&corev1.ResourceRequirements{}, 2, 528}, // no limit: the default's
		{importConsumerResources(), 0, 1056},     // a zero concurrency counts as one
	}
	for _, tc := range cases {
		got := importConsumerHeapMi(tc.res, tc.concurrency)
		if got != tc.want {
			t.Errorf("heap(%v, %d) = %d, want %d", tc.res.Limits.Memory(), tc.concurrency, got, tc.want)
		}
		if lim, ok := tc.res.Limits[corev1.ResourceMemory]; ok && tc.want > importConsumerMinHeapMi {
			c := max(int64(tc.concurrency), 1)
			if got*c+importConsumerWorkerOverheadMi > lim.Value()/(1024*1024) {
				t.Errorf("heap(%v, %d) = %d: the imports together exceed the limit", lim.String(), tc.concurrency, got)
			}
		}
	}
}

// A changed or created AWS Secret reconciles the stack that reads it, only while the consumer is on.
func TestImportConsumerAWSSecretIsWatched(t *testing.T) {
	if !slices.Contains(secretsReadBy(importConsumerOC(nil)), icAWSSecret) {
		t.Fatal("secretsReadBy omits the AWS credentials Secret of an enabled consumer")
	}
	off := importConsumerOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.ImportConsumer.Enabled = false })
	if slices.Contains(secretsReadBy(off), icAWSSecret) {
		t.Fatal("secretsReadBy names the AWS Secret of a disabled consumer")
	}
}

// The sandbox lets the consumer call the dashboard and nothing else in the stack, and lets it out to
// public HTTPS for SQS, S3 and the public writer host.
func TestNetworkSandboxAdmitsTheImportConsumer(t *testing.T) {
	oc := publicStack(nil)
	other := publicStack(func(o *oramav1alpha1.OramaCloudV1) { o.Name = "other" })
	h := newPublicHarness(t, oc)
	if err := h.r.reconcileNetworkSandbox(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	sandbox := &oramav1alpha1.NetworkSandbox{}
	if !h.get(networkSandboxName, sandbox) {
		t.Fatal("no NetworkSandbox")
	}
	consumer := labels.Set(labelsFor(oc, importConsumerComponent))
	dashboard := labels.Set(labelsFor(oc, "dashboard"))
	if !selectorMatches(t, sandbox.Spec.PodSelector, consumer) {
		t.Fatal("the sandbox's pod selector does not cover the consumer")
	}
	if !sandboxEgressAllows(t, sandbox, consumer, dashboard, 3000) || !sandboxIngressAllows(t, sandbox, consumer, dashboard, 3000) {
		t.Fatal("the consumer cannot reach the dashboard on 3000")
	}
	foreign := labels.Set(labelsFor(other, importConsumerComponent))
	if sandboxEgressAllows(t, sandbox, foreign, dashboard, 3000) || sandboxIngressAllows(t, sandbox, foreign, dashboard, 3000) {
		t.Fatal("another stack's consumer can reach this dashboard")
	}
	for comp, port := range map[string]int32{"writer": 8080, "cache": 6379, "broker": 5672, "analytics": 8123} {
		if sandboxEgressAllows(t, sandbox, consumer, labels.Set(labelsFor(oc, comp)), port) {
			t.Errorf("the consumer may reach %s on %d in the stack", comp, port)
		}
	}

	peers := []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}}
	sel := oramaCloudComponentSelector(oc, importConsumerComponent)
	all := oramaCloudPublicInternetAllowances(peers, sel, sel, sel, sel, sel, sel, sel, sel, sel, sel)
	i := slices.IndexFunc(all, func(p oramav1alpha1.NetworkSandboxPolicy) bool { return p.Name == "allow-import-consumer-public-https" })
	if i < 0 {
		t.Fatal("no allow-import-consumer-public-https allowance")
	}
	p := all[i].Spec
	if !selectorMatches(t, p.PodSelector, consumer) || len(p.Egress) != 1 || !portsAllowTCP(p.Egress[0].Ports, 443) || portsAllowTCP(p.Egress[0].Ports, 80) {
		t.Fatalf("allow-import-consumer-public-https = %+v, want the consumer out to 443 only", p)
	}
}

// TestImportConsumerAdmission checks the generated CRD's rules on a real API server.
func TestImportConsumerAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the OramaCloudV1 admission envtest")
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cl, err := client.New(cfg, client.Options{Scheme: ocv1TestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func(*oramav1alpha1.ImportConsumerSpec)
		err  string
	}{
		{name: "complete"},
		{name: "off and empty", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { *ic = oramav1alpha1.ImportConsumerSpec{} }},
		{name: "no image", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.Image = "" }, err: "importConsumer.image is required"},
		{name: "no queue", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.QueueURL = "" }, err: "importConsumer.queueURL is required"},
		{name: "no bucket", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.Bucket = "" }, err: "importConsumer.bucket is required"},
		{name: "no Secret", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.AWSCredentialsSecretRef = nil },
			err: "importConsumer.awsCredentialsSecretRef is required"},
		{name: "not an SQS URL", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.QueueURL = "https://example.com/q" },
			err: "spec.components.importConsumer.queueURL"},
		{name: "a bad bucket name", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.Bucket = "Bad_Bucket" },
			err: "spec.components.importConsumer.bucket"},
		{name: "too much concurrency", mut: func(ic *oramav1alpha1.ImportConsumerSpec) { ic.Concurrency = 17 },
			err: "spec.components.importConsumer.concurrency"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := importConsumerOC(func(o *oramav1alpha1.OramaCloudV1) {
				if tc.mut != nil {
					tc.mut(&o.Spec.Components.ImportConsumer)
				}
			})
			oc.Name, oc.Namespace, oc.UID = "ic-"+strconv.Itoa(i), "default", ""
			oc.Spec.IDP.Provider = "none"
			err := cl.Create(context.Background(), oc)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.err != "" && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("error = %v, want an Invalid error containing %q", err, tc.err)
			case tc.err == "" && oc.Spec.Components.ImportConsumer.Enabled:
				ic := oc.Spec.Components.ImportConsumer
				if ic.Region != "us-east-1" || ic.Concurrency != 2 || ic.Replicas != 1 {
					t.Fatalf("defaults region %q concurrency %d replicas %d", ic.Region, ic.Concurrency, ic.Replicas)
				}
			}
		})
	}
}
