// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// combinedConfigFixture is config.yaml for a CR named v1 with a CPU writer and no spec.llm, captured
// from the renderer before it was split into sections, with the LLM section rendered from the default
// providers (#304), the master key single-quoted for safe substitution, Prod 1's hooks block on
// both sides (#306), the broker password as a placeholder (#307), and both sections signed in as
// oramacore-writer, since the combined image produces from every pod (#99). The combined config must stay byte-for-byte this.
const combinedConfigFixture = `http:
  host: 0.0.0.0
  port: 8080
  allow_cors: true
  with_prometheus: true
log: {}
writer_side:
  output:
    type: rabbitmq
    host: v1-broker
    port: 5552
    user: oramacore-writer
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-writer
    producer_name: write
` + v1HooksFixture + `  master_api_key: '__MASTER_API_KEY__'
  config:
    data_dir: /app/.data/writer
    embedding_queue_limit: 64
    insert_batch_commit_size: 50000000
    default_embedding_model: BGESmall
    javascript_queue_limit: 500000
    commit_interval: 1m
reader_side:
  input:
    type: rabbitmq
    host: v1-broker
    port: 5552
    user: oramacore-writer
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-reader
    consumer_name: reader
` + v1HooksFixture + `  config:
    data_dir: /app/.data/reader
    insert_batch_commit_size: 50000000
    commit_interval: 1m
ai_server:
  scheme: http
  host: v1-embeddings
  port: 50051
  api_key: ""
  max_connections: 15
  total_threads: 12
  embeddings:
    default_model_group: small
    dynamically_load_models: false
    execution_providers:
      - CPUExecutionProvider
    total_threads: 8
  llm:
    local: false
    host: "https://api.groq.com/openai/v1"
    model: "openai/gpt-oss-120b"
    api_key: '__GROQ_API_KEY__'
  remote_llms:
    - provider: openai
      api_key: '__OPENAI_API_KEY__'
      default_model: "gpt-5.2"
    - provider: google
      api_key: '__GOOGLE_API_KEY__'
      default_model: "gemini-2.0-flash-001"
    - provider: groq
      api_key: '__GROQ_API_KEY__'
      default_model: "openai/gpt-oss-120b"
    - provider: anthropic
      api_key: '__ANTHROPIC_API_KEY__'
      default_model: "claude-sonnet-4-5"
`

// combinedReaderConfigFixture is combined-reader.yaml for the same CR (#795): the combined
// config with the writer's master key replaced by a key render-config generates, both sections
// signed in as oramacore-reader, the writer section producing to oramacore-reader-local, and the
// reader section recording analytics (#1051).
const combinedReaderConfigFixture = `http:
  host: 0.0.0.0
  port: 8080
  allow_cors: true
  with_prometheus: true
log: {}
writer_side:
  output:
    type: rabbitmq
    host: v1-broker
    port: 5552
    user: oramacore-reader
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-reader-local
    client_provided_name: oramacore-reader-local
    producer_name: write
` + v1HooksFixture + `  master_api_key: '__LOCAL_WRITER_KEY__'
  config:
    data_dir: /app/.data/writer
    embedding_queue_limit: 64
    insert_batch_commit_size: 50000000
    default_embedding_model: BGESmall
    javascript_queue_limit: 500000
    commit_interval: 1m
reader_side:
  input:
    type: rabbitmq
    host: v1-broker
    port: 5552
    user: oramacore-reader
    password: '__BROKER_PASSWORD__'
    v_host: /
    stream_name: oramacore-operations
    client_provided_name: oramacore-reader
    consumer_name: reader
` + v1HooksFixture + `  config:
    data_dir: /app/.data/reader
    insert_batch_commit_size: 50000000
    commit_interval: 1m
  analytics:
    api_key: '__ANALYTICS_API_KEY__'
    metadata_from_headers:
      - header: user-agent
        metadata_key: ua
      - header: CF-Connecting-IP
        metadata_key: ip
      - header: CF-IPCountry
        metadata_key: country
      - header: continent
        metadata_key: continent
      - header: colo
        metadata_key: colo
      - header: latitude
        metadata_key: latitude
      - header: longitude
        metadata_key: longitude
ai_server:
  scheme: http
  host: v1-embeddings
  port: 50051
  api_key: ""
  max_connections: 15
  total_threads: 12
  embeddings:
    default_model_group: small
    dynamically_load_models: false
    execution_providers:
      - CPUExecutionProvider
    total_threads: 8
  llm:
    local: false
    host: "https://api.groq.com/openai/v1"
    model: "openai/gpt-oss-120b"
    api_key: '__GROQ_API_KEY__'
  remote_llms:
    - provider: openai
      api_key: '__OPENAI_API_KEY__'
      default_model: "gpt-5.2"
    - provider: google
      api_key: '__GOOGLE_API_KEY__'
      default_model: "gemini-2.0-flash-001"
    - provider: groq
      api_key: '__GROQ_API_KEY__'
      default_model: "openai/gpt-oss-120b"
    - provider: anthropic
      api_key: '__ANTHROPIC_API_KEY__'
      default_model: "claude-sonnet-4-5"
`

// combinedRenderCommand is the render-config script of the writer without side-only images and with
// the default providers.
const combinedRenderCommand = `esc() { printf '%s' "$1" | sed -e "s/'/''/g" -e 's/[\\&#]/\\&/g'; }; sed` +
	` -e "s#__MASTER_API_KEY__#$(esc "${MASTER_API_KEY}")#g"` +
	` -e "s#__BROKER_PASSWORD__#$(esc "${BROKER_PASSWORD}")#g"` +
	` -e "s#__OPENAI_API_KEY__#$(esc "${OPENAI_API_KEY}")#g"` +
	` -e "s#__GOOGLE_API_KEY__#$(esc "${GOOGLE_API_KEY}")#g"` +
	` -e "s#__GROQ_API_KEY__#$(esc "${GROQ_API_KEY}")#g"` +
	` -e "s#__ANTHROPIC_API_KEY__#$(esc "${ANTHROPIC_API_KEY}")#g"` +
	` /tmpl/config.yaml > /etc/oramacore/config.yaml`

// combinedReaderRenderCommand is the readers' render-config script without side-only images and with
// the default providers: it generates the local writer key and fills no master key, but the analytics key.
const combinedReaderRenderCommand = `esc() { printf '%s' "$1" | sed -e "s/'/''/g" -e 's/[\\&#]/\\&/g'; }; ` +
	`LOCAL_WRITER_KEY=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'); sed` +
	` -e "s#__LOCAL_WRITER_KEY__#$(esc "${LOCAL_WRITER_KEY}")#g"` +
	` -e "s#__BROKER_PASSWORD__#$(esc "${BROKER_PASSWORD}")#g"` +
	` -e "s#__ANALYTICS_API_KEY__#$(esc "${ANALYTICS_API_KEY}")#g"` +
	` -e "s#__OPENAI_API_KEY__#$(esc "${OPENAI_API_KEY}")#g"` +
	` -e "s#__GOOGLE_API_KEY__#$(esc "${GOOGLE_API_KEY}")#g"` +
	` -e "s#__GROQ_API_KEY__#$(esc "${GROQ_API_KEY}")#g"` +
	` -e "s#__ANTHROPIC_API_KEY__#$(esc "${ANTHROPIC_API_KEY}")#g"` +
	` /tmpl/combined-reader.yaml > /etc/oramacore/config.yaml`

const (
	testWriterImage = "oramasearch/oramacore-writer@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testReaderImage = "oramasearch/oramacore-reader@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// engineHarness is a fake cluster for the engine, counting deletes and status writes.
type engineHarness struct {
	t            *testing.T
	cl           client.Client
	r            *OramaCloudV1Reconciler
	deletes      int
	statusWrites int
	// updates counts Update calls by object name.
	updates map[string]int
}

// testMasterSecret is the master API key Secret engineOC references; newEngineHarness adds it unless
// the test supplies its own.
const testMasterSecret = "v1-master"

// testMasterKey is testMasterSecret's MASTER_API_KEY, different from the stale value engineKeys
// leaves in the engine Secret.
const testMasterKey = "shared-master-0099"

func newEngineHarness(t *testing.T, objs ...client.Object) *engineHarness {
	t.Helper()
	s := ocv1TestScheme(t)
	h := &engineHarness{t: t, updates: map[string]int{}}
	if !slices.ContainsFunc(objs, func(o client.Object) bool { _, ok := o.(*corev1.Secret); return ok && o.GetName() == testMasterSecret }) {
		objs = append(objs, secretOf(testMasterSecret, map[string]string{"MASTER_API_KEY": testMasterKey}))
	}
	// There is no RabbitMQTopology controller here: the stack's topology reports its users applied,
	// as on a cluster, so the engine is written (brokerUsersReady).
	if !slices.ContainsFunc(objs, func(o client.Object) bool { _, ok := o.(*oramav1alpha1.RabbitMQTopology); return ok }) {
		objs = append(objs, readyBrokerTopology("v1"))
	}
	h.cl = fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				h.deletes++
				return c.Delete(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				h.updates[obj.GetName()]++
				return c.Update(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				h.statusWrites++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	h.r = &OramaCloudV1Reconciler{Client: h.cl, Scheme: s}
	return h
}

// harnessObjects is how many items of list newEngineHarness creates itself: testMasterSecret.
func harnessObjects(list client.ObjectList) int {
	if _, ok := list.(*corev1.SecretList); ok {
		return 1
	}
	return 0
}

func (h *engineHarness) reconcile(oc *oramav1alpha1.OramaCloudV1) {
	h.t.Helper()
	if err := h.r.reconcileEngine(context.Background(), oc); err != nil {
		h.t.Fatalf("reconcileEngine: %v", err)
	}
}

// get reads name into obj and reports whether it exists.
func (h *engineHarness) get(name string, obj client.Object) bool {
	h.t.Helper()
	err := h.cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "orama-system"}, obj)
	if err != nil && client.IgnoreNotFound(err) != nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

func (h *engineHarness) writer() *appsv1.StatefulSet {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get("v1-writer", sts) {
		h.t.Fatal("writer StatefulSet v1-writer not found")
	}
	return sts
}

func (h *engineHarness) reader() *appsv1.StatefulSet {
	h.t.Helper()
	sts := &appsv1.StatefulSet{}
	if !h.get("v1-reader", sts) {
		h.t.Fatal("reader StatefulSet v1-reader not found")
	}
	return sts
}

func (h *engineHarness) engineConfig() *corev1.ConfigMap {
	h.t.Helper()
	cm := &corev1.ConfigMap{}
	if !h.get("v1-engine-config", cm) {
		h.t.Fatal("ConfigMap v1-engine-config not found")
	}
	return cm
}

// engineOC is a primary CR named v1 with the engine enabled and its master key in testMasterSecret.
func engineOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Components.Engine.Enabled = true
		o.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: testMasterSecret}
		if mut != nil {
			mut(o)
		}
	})
}

func withSideImages(o *oramav1alpha1.OramaCloudV1) {
	if o.Spec.Images.Overrides == nil {
		o.Spec.Images.Overrides = map[string]string{}
	}
	o.Spec.Images.Overrides["oramacore-writer"] = testWriterImage
	o.Spec.Images.Overrides["oramacore-reader"] = testReaderImage
}

func container(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func renderInit(t *testing.T, spec corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, c := range spec.InitContainers {
		if c.Name == "render-config" {
			return c
		}
	}
	t.Fatal("no render-config init container")
	return corev1.Container{}
}

var renderSourceRe = regexp.MustCompile(` /tmpl/(\S+) > /etc/oramacore/config\.yaml$`)

// renderSource is the ConfigMap key the pod's render-config init container renders.
func renderSource(t *testing.T, spec corev1.PodSpec) string {
	t.Helper()
	m := renderSourceRe.FindStringSubmatch(renderInit(t, spec).Command[2])
	if m == nil {
		t.Fatalf("render-config command %q does not render into /etc/oramacore/config.yaml", renderInit(t, spec).Command[2])
	}
	return m[1]
}

func parseYAML(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("config does not parse as YAML: %v", err)
	}
	return m
}

func templateSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func dataKeys(cm *corev1.ConfigMap) []string {
	var keys []string
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestEngineImageSelection(t *testing.T) {
	cases := []struct {
		name         string
		images       oramav1alpha1.ImagesSpec
		writer, read string
	}{
		{"nil overrides", oramav1alpha1.ImagesSpec{}, "oramasearch/oramacore:latest", "oramasearch/oramacore:latest"},
		{"empty overrides", oramav1alpha1.ImagesSpec{Overrides: map[string]string{}}, "oramasearch/oramacore:latest", "oramasearch/oramacore:latest"},
		{"empty side values", oramav1alpha1.ImagesSpec{Overrides: map[string]string{"oramacore-writer": "", "oramacore-reader": ""}},
			"oramasearch/oramacore:latest", "oramasearch/oramacore:latest"},
		{"global tag does not select the combined image", oramav1alpha1.ImagesSpec{Tag: "v1-sandbox"},
			"oramasearch/oramacore:latest", "oramasearch/oramacore:latest"},
		{"custom registry", oramav1alpha1.ImagesSpec{Registry: "registry.example.com/orama", Tag: "v1-sandbox"},
			"registry.example.com/orama/oramacore:latest", "registry.example.com/orama/oramacore:latest"},
		{"combined override", oramav1alpha1.ImagesSpec{Overrides: map[string]string{"oramacore": "example.invalid/combined:tag"}},
			"example.invalid/combined:tag", "example.invalid/combined:tag"},
		{"side overrides", oramav1alpha1.ImagesSpec{Overrides: map[string]string{
			"oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}}, testWriterImage, testReaderImage},
		{"side overrides win over the combined override", oramav1alpha1.ImagesSpec{Overrides: map[string]string{
			"oramacore": "example.invalid/combined:tag", "oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}},
			testWriterImage, testReaderImage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images = tc.images })
			if got := engineSideFor(oc, "writer").image; got != tc.writer {
				t.Errorf("writer image = %q, want %q", got, tc.writer)
			}
			if got := engineSideFor(oc, "reader").image; got != tc.read {
				t.Errorf("reader image = %q, want %q", got, tc.read)
			}
		})
	}
}

// Without both side overrides the writer renders the unchanged combined config and the readers the
// combined reader config (#795), each hashed like a side's.
func TestEngineWithoutSideImagesIsUnchanged(t *testing.T) {
	for name, overrides := range map[string]map[string]string{
		"nil":          nil,
		"empty":        {},
		"empty values": {"oramacore-writer": "", "oramacore-reader": ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEngineHarness(t)
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.Overrides = overrides })
			h.reconcile(oc)

			cm := h.engineConfig()
			if got := cm.Data["config.yaml"]; got != combinedConfigFixture {
				t.Errorf("config.yaml changed from the captured rendering:\n%s", got)
			}
			if got := cm.Data["combined-reader.yaml"]; got != combinedReaderConfigFixture {
				t.Errorf("combined-reader.yaml changed from the captured rendering:\n%s", got)
			}
			if got, want := dataKeys(cm), []string{"combined-reader.yaml", "config.yaml", "envoy.yaml"}; !reflect.DeepEqual(got, want) {
				t.Errorf("ConfigMap keys = %v, want %v", got, want)
			}
			for _, p := range []struct {
				side, command, fixture string
				spec                   corev1.PodTemplateSpec
			}{
				{"writer", combinedRenderCommand, combinedConfigFixture, h.writer().Spec.Template},
				{"reader", combinedReaderRenderCommand, combinedReaderConfigFixture, h.reader().Spec.Template},
			} {
				if got := container(p.spec.Spec, "oramacore").Image; got != "oramasearch/oramacore:latest" {
					t.Errorf("%s image = %q, want the combined image", p.side, got)
				}
				if got := renderInit(t, p.spec.Spec).Command[2]; got != p.command {
					t.Errorf("%s render-config command = %q, want %q", p.side, got, p.command)
				}
				if got, want := p.spec.Annotations[configHashKey], templateSHA256(p.fixture); got != want {
					t.Errorf("%s config hash = %q, want SHA-256 of its template %q", p.side, got, want)
				}
			}
		})
	}
}

func TestEngineSideOnlyPodsAndConfigs(t *testing.T) {
	h := newEngineHarness(t)
	oc := engineOC(withSideImages)
	h.reconcile(oc)

	cm := h.engineConfig()
	if got, want := dataKeys(cm), []string{"envoy.yaml", "reader.yaml", "writer.yaml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfigMap keys = %v, want %v", got, want)
	}
	combined := parseYAML(t, combinedConfigFixture)

	pods := []struct {
		side, image, key, absent, present string
		spec                              corev1.PodTemplateSpec
	}{
		{"writer", testWriterImage, "writer.yaml", "reader_side", "writer_side", h.writer().Spec.Template},
		{"reader", testReaderImage, "reader.yaml", "writer_side", "reader_side", h.reader().Spec.Template},
	}
	for _, p := range pods {
		c := container(p.spec.Spec, "oramacore")
		if c.Image != p.image {
			t.Errorf("%s image = %q, want %q", p.side, c.Image, p.image)
		}
		if got := renderSource(t, p.spec.Spec); got != p.key {
			t.Errorf("%s renders %q, want %q", p.side, got, p.key)
		}
		if !reflect.DeepEqual(c.Env, []corev1.EnvVar{{Name: "RUST_LOG", Value: "oramacore=info,warn"}, {Name: "CONFIG_PATH", Value: "/etc/oramacore/config.yaml"}}) {
			t.Errorf("%s oramacore env = %v", p.side, c.Env)
		}
		for _, ic := range append(append([]corev1.Container{}, p.spec.Spec.InitContainers...), p.spec.Spec.Containers...) {
			for _, e := range ic.Env {
				if e.Name == "POD_NAME" {
					t.Errorf("%s container %s has a POD_NAME env entry", p.side, ic.Name)
				}
			}
		}
		if got := renderInit(t, p.spec.Spec).EnvFrom; !reflect.DeepEqual(got, envFromFor(oc, "engine")) {
			t.Errorf("%s render-config envFrom = %v, want the engine envFrom", p.side, got)
		}
		if got, want := p.spec.Annotations[configHashKey], templateSHA256(cm.Data[p.key]); got != want {
			t.Errorf("%s config hash = %q, want SHA-256 of %s %q", p.side, got, p.key, want)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.spec.Annotations[configHashKey]) {
			t.Errorf("%s config hash %q is not 64 lowercase hex characters", p.side, p.spec.Annotations[configHashKey])
		}

		tmpl := cm.Data[p.key]
		if !strings.HasSuffix(tmpl, "\n") || strings.Contains(tmpl, "__POD_NAME__") {
			t.Errorf("%s template must end in a newline and hold no __POD_NAME__", p.key)
		}
		parsed := parseYAML(t, tmpl)
		// The reader-only config alone adds reader_side.analytics (engine_analytics_test.go).
		if rs, ok := parsed["reader_side"].(map[string]any); ok && p.side == "reader" {
			if _, ok := rs["analytics"]; !ok {
				t.Error("reader.yaml has no reader_side.analytics")
			}
			delete(rs, "analytics")
			// A reader-only pod signs in as oramacore-reader; the combined config's readers, which
			// also produce, as oramacore-writer (#99).
			if in, ok := rs["input"].(map[string]any); ok {
				if in["user"] != brokerReaderUser {
					t.Errorf("reader.yaml reader_side.input.user = %v, want %s", in["user"], brokerReaderUser)
				}
				in["user"] = brokerWriterUser
			}
		}
		if _, ok := parsed[p.absent]; ok {
			t.Errorf("%s has %s", p.key, p.absent)
		}
		if _, ok := parsed[p.present]; !ok {
			t.Errorf("%s has no %s", p.key, p.present)
		}
		// Every other section and value is the combined config's: the stream names, broker and
		// embeddings hosts, data directories and credential placeholders.
		want := map[string]any{}
		for k, v := range combined {
			if k != p.absent {
				want[k] = v
			}
		}
		if !reflect.DeepEqual(parsed, want) {
			t.Errorf("%s differs from the combined config without %s:\n%s", p.key, p.absent, tmpl)
		}
	}

	writer, reader := cm.Data["writer.yaml"], cm.Data["reader.yaml"]
	for _, s := range []string{"client_provided_name: oramacore-writer", "producer_name: write", "host: v1-broker",
		"data_dir: /app/.data/writer", `master_api_key: '__MASTER_API_KEY__'`, "host: v1-embeddings", `api_key: '__GROQ_API_KEY__'`} {
		if !strings.Contains(writer, s) {
			t.Errorf("writer.yaml lacks %q", s)
		}
	}
	for _, s := range []string{"client_provided_name: oramacore-reader", "consumer_name: reader", "host: v1-broker",
		"data_dir: /app/.data/reader", "host: v1-embeddings", `api_key: '__GROQ_API_KEY__'`} {
		if !strings.Contains(reader, s) {
			t.Errorf("reader.yaml lacks %q", s)
		}
	}
}

// One side-only image without the other is refused before anything is written.
func TestEngineImagesBothOrNeither(t *testing.T) {
	const want = "spec.images.overrides needs both oramacore-writer and oramacore-reader, or neither"
	cases := map[string]map[string]string{
		"writer only, reader missing": {"oramacore-writer": testWriterImage},
		"writer only, reader empty":   {"oramacore-writer": testWriterImage, "oramacore-reader": ""},
		"reader only, writer missing": {"oramacore-reader": testReaderImage},
		"reader only, writer empty":   {"oramacore-reader": testReaderImage, "oramacore-writer": ""},
	}
	for name, overrides := range cases {
		t.Run(name+" on a fresh stack", func(t *testing.T) {
			h := newEngineHarness(t)
			oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.Overrides = overrides })
			err := h.r.reconcileEngine(context.Background(), oc)
			if err == nil || err.Error() != want {
				t.Fatalf("reconcileEngine error = %v, want %q", err, want)
			}
			for _, list := range []client.ObjectList{&corev1.ConfigMapList{}, &corev1.SecretList{}, &corev1.ServiceList{},
				&appsv1.StatefulSetList{}, &appsv1.DeploymentList{}} {
				if err := h.cl.List(context.Background(), list); err != nil {
					t.Fatal(err)
				}
				if n := meta.LenList(list) - harnessObjects(list); n != 0 {
					t.Errorf("%T has %d items after a refused reconcile, want 0", list, n)
				}
			}
		})
		for _, from := range []string{"combined", "separated"} {
			t.Run(name+" on an existing "+from+" stack", func(t *testing.T) {
				h := newEngineHarness(t)
				oc := engineOC(nil)
				if from == "separated" {
					withSideImages(oc)
				}
				h.reconcile(oc)
				cm, sts, readers := h.engineConfig(), h.writer(), h.reader()

				oc.Spec.Images.Overrides = overrides
				if err := h.r.reconcileEngine(context.Background(), oc); err == nil || err.Error() != want {
					t.Fatalf("reconcileEngine error = %v, want %q", err, want)
				}
				if got := h.engineConfig(); got.ResourceVersion != cm.ResourceVersion || !reflect.DeepEqual(got.Data, cm.Data) {
					t.Error("the refused reconcile changed the engine ConfigMap")
				}
				if got := h.writer(); got.ResourceVersion != sts.ResourceVersion || !reflect.DeepEqual(got.Spec, sts.Spec) {
					t.Error("the refused reconcile changed the writer StatefulSet")
				}
				if got := h.reader(); got.ResourceVersion != readers.ResourceVersion || !reflect.DeepEqual(got.Spec, readers.Spec) {
					t.Error("the refused reconcile changed the reader StatefulSet")
				}
			})
		}
	}
}

// The controller checks the images before the network sandbox and bootstrap write anything, and
// reports the refusal through the Ready condition without inventing a separation condition.
func TestReconcileRefusesOneSideImageBeforeAnyChange(t *testing.T) {
	old := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Generation = 2
		o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage}
		o.Status.Conditions = []metav1.Condition{{Type: engineSidesConditionType, Status: metav1.ConditionFalse,
			Reason: "CombinedImage", Message: "earlier", ObservedGeneration: 1, LastTransitionTime: old}}
	})
	h := newEngineHarness(t, oc)
	_, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v1", Namespace: "orama-system"}})
	if err == nil {
		t.Fatal("Reconcile accepted a single side-only image")
	}
	for _, list := range []client.ObjectList{&oramav1alpha1.NetworkSandboxList{}, &corev1.ConfigMapList{}, &corev1.SecretList{},
		&appsv1.StatefulSetList{}, &appsv1.DeploymentList{}} {
		if err := h.cl.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if n := meta.LenList(list) - harnessObjects(list); n != 0 {
			t.Errorf("%T has %d items after a refused reconcile, want 0", list, n)
		}
	}

	got := &oramav1alpha1.OramaCloudV1{}
	h.get("v1", got)
	if got.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", got.Status.Phase)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileError" ||
		ready.Message != "engine: spec.images.overrides needs both oramacore-writer and oramacore-reader, or neither" {
		t.Errorf("Ready = %+v, want False/ReconcileError naming the images", ready)
	}
	sides := meta.FindStatusCondition(got.Status.Conditions, engineSidesConditionType)
	if sides == nil || sides.Message != "earlier" || sides.ObservedGeneration != 1 || !sides.LastTransitionTime.Equal(&old) {
		t.Errorf("%s = %+v, want the earlier condition left as it was", engineSidesConditionType, sides)
	}
}

// A disabled engine is torn down even with a leftover single override.
func TestEngineTeardownIgnoresOneSideImage(t *testing.T) {
	h := newEngineHarness(t)
	oc := engineOC(nil)
	h.reconcile(oc)

	oc.Spec.Components.Engine.Enabled = false
	oc.Spec.Images.Overrides = map[string]string{"oramacore-reader": testReaderImage}
	if err := validateEngineImages(oc); err != nil {
		t.Fatalf("validateEngineImages on a disabled engine: %v", err)
	}
	h.reconcile(oc)
	if h.get("v1-writer", &appsv1.StatefulSet{}) || h.get("v1-reader", &appsv1.StatefulSet{}) || h.get("v1-engine-config", &corev1.ConfigMap{}) {
		t.Error("the disabled engine's writer, readers or ConfigMap survived teardown")
	}
}

func TestEngineSidesConditionPersisted(t *testing.T) {
	old := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	other := metav1.Condition{Type: "BackupReady", Status: metav1.ConditionTrue, Reason: "Kept", Message: "kept",
		ObservedGeneration: 1, LastTransitionTime: old}
	stored := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Generation = 3
		withSideImages(o)
		o.Status.Conditions = []metav1.Condition{other, {Type: engineSidesConditionType, Status: metav1.ConditionTrue,
			Reason: "SideImages", Message: "older message", ObservedGeneration: 2, LastTransitionTime: old}}
	})
	// A reader that is not ready keeps the stack Progressing whatever the images are.
	notReady := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "v1-reader", Namespace: "orama-system", Labels: labelsFor(stored, "reader")},
		Spec: appsv1.StatefulSetSpec{Replicas: int32Ptr(1)}}
	h := newEngineHarness(t, stored, notReady)

	read := func() *oramav1alpha1.OramaCloudV1 {
		oc := &oramav1alpha1.OramaCloudV1{}
		h.get("v1", oc)
		return oc
	}
	apply := func(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
		oc := read()
		if mut != nil {
			mut(oc)
		}
		h.r.applyReadiness(context.Background(), oc, nil)
		return read()
	}
	check := func(oc *oramav1alpha1.OramaCloudV1, status metav1.ConditionStatus, reason string) *metav1.Condition {
		t.Helper()
		if oc.Status.Phase != "Progressing" || oc.Status.ComponentsReady != "0/1" {
			t.Errorf("readiness = %s %s, want Progressing 0/1", oc.Status.Phase, oc.Status.ComponentsReady)
		}
		if c := meta.FindStatusCondition(oc.Status.Conditions, "BackupReady"); c == nil || !c.LastTransitionTime.Equal(&other.LastTransitionTime) ||
			c.Status != other.Status || c.Reason != other.Reason || c.Message != other.Message || c.ObservedGeneration != other.ObservedGeneration {
			t.Errorf("BackupReady = %+v, want it preserved", c)
		}
		c := meta.FindStatusCondition(oc.Status.Conditions, engineSidesConditionType)
		if c == nil || c.Status != status || c.Reason != reason || c.ObservedGeneration != oc.Generation {
			t.Fatalf("%s = %+v, want %s/%s at generation %d", engineSidesConditionType, c, status, reason, oc.Generation)
		}
		return c
	}

	oc := apply(nil)
	c := check(oc, metav1.ConditionTrue, "SideImages")
	if !c.LastTransitionTime.Equal(&old) {
		t.Errorf("LastTransitionTime = %v, want the stored %v kept", c.LastTransitionTime, old)
	}
	if h.statusWrites != 1 {
		t.Fatalf("status writes = %d, want 1", h.statusWrites)
	}
	apply(nil)
	if h.statusWrites != 1 {
		t.Errorf("an identical reconcile wrote status (%d writes)", h.statusWrites)
	}

	// Only the separation changes: one write, the readiness fields stay.
	oc = apply(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.Overrides = nil })
	c = check(oc, metav1.ConditionFalse, "CombinedImage")
	if !strings.Contains(c.Message, "every reader also runs a writer") ||
		!strings.Contains(c.Message, "oramacore-writer") || !strings.Contains(c.Message, "oramacore-reader") {
		t.Errorf("CombinedImage message = %q", c.Message)
	}
	if h.statusWrites != 2 {
		t.Errorf("status writes = %d, want 2", h.statusWrites)
	}
	transition := c.LastTransitionTime
	oc = apply(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.Overrides = nil })
	if h.statusWrites != 2 {
		t.Errorf("an identical reconcile wrote status (%d writes)", h.statusWrites)
	}
	if c := meta.FindStatusCondition(oc.Status.Conditions, engineSidesConditionType); !c.LastTransitionTime.Equal(&transition) {
		t.Errorf("LastTransitionTime moved on an identical reconcile")
	}

	// Disabling the engine removes the condition, and that alone is written.
	oc = apply(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false })
	if meta.FindStatusCondition(oc.Status.Conditions, engineSidesConditionType) != nil {
		t.Error("the condition survived disabling the engine")
	}
	if meta.FindStatusCondition(oc.Status.Conditions, "BackupReady") == nil || oc.Status.Phase != "Progressing" {
		t.Error("disabling the engine dropped another condition or changed the phase")
	}
	if h.statusWrites != 3 {
		t.Errorf("status writes = %d, want 3", h.statusWrites)
	}
	apply(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Enabled = false })
	if h.statusWrites != 3 {
		t.Errorf("an identical reconcile wrote status (%d writes)", h.statusWrites)
	}
}

// Each side's hash covers only its own template: a reader-only change rolls only readers.
func TestEngineConfigHashIsPerSide(t *testing.T) {
	oc := engineOC(withSideImages)
	writer, reader := engineSideFor(oc, "writer"), engineSideFor(oc, "reader")
	hash := func(s engineSide) string { return engineConfigAnnotations(s, "", "", "")[configHashKey] }

	// Edit one side's template the way a side-only setting would; only that side's hash moves.
	before := map[string]string{"writer": hash(writer), "reader": hash(reader)}
	for _, edit := range []string{"reader", "writer"} {
		sides := map[string]engineSide{"writer": writer, "reader": reader}
		edited := sides[edit]
		if n := strings.Count(edited.template, "commit_interval: 1m"); n != 1 {
			t.Fatalf("%s template has %d commit_interval: 1m, want 1", edit, n)
		}
		edited.template = strings.Replace(edited.template, "commit_interval: 1m", "commit_interval: 2m", 1)
		sides[edit] = edited
		for side, es := range sides {
			if moved := hash(es) != before[side]; moved != (side == edit) {
				t.Errorf("editing the %s template: %s hash changed = %v", edit, side, moved)
			}
		}
	}
	if combined := engineSideFor(engineOC(nil), "writer"); hash(combined) != templateSHA256(combined.template) {
		t.Error("the combined config is not hashed")
	}

	h := newEngineHarness(t)
	h.reconcile(oc)
	writerTemplate := h.writer().Spec.Template
	readerHash := h.reader().Spec.Template.Annotations[configHashKey]

	// Reader image, replicas and resources change the reader pod, not either config.
	oc.Spec.Images.Overrides["oramacore-reader"] = "oramasearch/oramacore-reader@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	oc.Spec.Components.Engine.Reader.Replicas = 5
	oc.Spec.Components.Engine.Reader.Resources = resourceCaps("3Gi", "2", "1Gi", "500m")
	h.reconcile(oc)
	if !reflect.DeepEqual(h.writer().Spec.Template, writerTemplate) {
		t.Error("a reader-only change changed the writer pod template")
	}
	readers := h.reader()
	if readers.Spec.Template.Annotations[configHashKey] != readerHash {
		t.Error("a reader image, replica or resource change changed the reader config hash")
	}
	if *readers.Spec.Replicas != 5 || container(readers.Spec.Template.Spec, "oramacore").Image != oc.Spec.Images.Overrides["oramacore-reader"] ||
		!container(readers.Spec.Template.Spec, "oramacore").Resources.Limits.Memory().Equal(resource.MustParse("3Gi")) {
		t.Error("the reader change was not applied")
	}

	// writer.gpu switches the shared ai_server execution provider, so both configs change.
	oc.Spec.Components.Engine.Writer.GPU = true
	h.reconcile(oc)
	cm := h.engineConfig()
	if got := h.writer().Spec.Template.Annotations[configHashKey]; got == writerTemplate.Annotations[configHashKey] || got != templateSHA256(cm.Data["writer.yaml"]) {
		t.Error("writer.gpu did not roll the writer to the new writer.yaml")
	}
	if got := h.reader().Spec.Template.Annotations[configHashKey]; got == readerHash || got != templateSHA256(cm.Data["reader.yaml"]) {
		t.Error("writer.gpu did not roll the readers to the new reader.yaml")
	}
}

// Switching modes updates the existing workloads in place and drops the other mode's keys.
func TestEngineModeTransitionsInPlace(t *testing.T) {
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-v1-writer-0", Namespace: "orama-system"}}
	h := newEngineHarness(t, claim)
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Engine.Envoy = true })

	expect := func(mode string, keys []string, writerImage, readerImage, writerKey, readerKey string) {
		t.Helper()
		cm := h.engineConfig()
		if got := dataKeys(cm); !reflect.DeepEqual(got, keys) {
			t.Errorf("%s: ConfigMap keys = %v, want %v", mode, got, keys)
		}
		if cm.Data["envoy.yaml"] != envoyConfig {
			t.Errorf("%s: envoy.yaml changed", mode)
		}
		for _, p := range []struct {
			side, image, key string
			spec             corev1.PodTemplateSpec
		}{{"writer", writerImage, writerKey, h.writer().Spec.Template}, {"reader", readerImage, readerKey, h.reader().Spec.Template}} {
			if got := container(p.spec.Spec, "oramacore").Image; got != p.image {
				t.Errorf("%s: %s image = %q, want %q", mode, p.side, got, p.image)
			}
			if got := renderSource(t, p.spec.Spec); got != p.key {
				t.Errorf("%s: %s renders %q, want %q", mode, p.side, got, p.key)
			}
			// The Envoy readers' hash also covers the bootstrap (#314); the writer's is its template's.
			want := templateSHA256(cm.Data[p.key])
			if p.side == "reader" {
				want = sha256Hex([]byte(cm.Data[p.key]), []byte(cm.Data["envoy.yaml"]))
			}
			if got := p.spec.Annotations[configHashKey]; got != want {
				t.Errorf("%s: %s config hash = %q, want the hash of %s", mode, p.side, got, p.key)
			}
		}
		if container(h.reader().Spec.Template.Spec, "envoy") == nil {
			t.Errorf("%s: the reader lost its Envoy sidecar", mode)
		}
		if len(h.writer().Spec.VolumeClaimTemplates) != 1 || h.writer().Spec.VolumeClaimTemplates[0].Name != "data" {
			t.Errorf("%s: the writer's data claim template changed", mode)
		}
		if !h.get(claim.Name, &corev1.PersistentVolumeClaim{}) {
			t.Errorf("%s: the writer's claim is gone", mode)
		}
		if h.deletes != 0 {
			t.Errorf("%s: %d objects were deleted", mode, h.deletes)
		}
	}

	h.reconcile(oc)
	expect("combined", []string{"combined-reader.yaml", "config.yaml", "envoy.yaml"}, "oramasearch/oramacore:latest", "oramasearch/oramacore:latest", "config.yaml", "combined-reader.yaml")
	writerUID := h.writer().UID
	withSideImages(oc)
	h.reconcile(oc)
	expect("separated", []string{"envoy.yaml", "reader.yaml", "writer.yaml"}, testWriterImage, testReaderImage, "writer.yaml", "reader.yaml")
	oc.Spec.Images.Overrides = nil
	h.reconcile(oc)
	expect("combined again", []string{"combined-reader.yaml", "config.yaml", "envoy.yaml"}, "oramasearch/oramacore:latest", "oramasearch/oramacore:latest", "config.yaml", "combined-reader.yaml")
	if h.writer().UID != writerUID {
		t.Error("the writer StatefulSet was recreated")
	}

	oc.Spec.Components.Engine.Envoy = false
	h.reconcile(oc)
	if container(h.reader().Spec.Template.Spec, "envoy") != nil {
		t.Error("the reader kept Envoy after engine.envoy was turned off")
	}
}

// A read region runs readers only; they still take the reader image and config.
func TestEngineReadRegionRunsReaderOnly(t *testing.T) {
	h := newEngineHarness(t)
	oc := engineOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role = "read"
		withSideImages(o)
	})
	h.reconcile(oc)
	if h.get("v1-writer", &appsv1.StatefulSet{}) {
		t.Error("a read region created a writer")
	}
	spec := h.reader().Spec.Template
	if got := container(spec.Spec, "oramacore").Image; got != testReaderImage {
		t.Errorf("reader image = %q, want %q", got, testReaderImage)
	}
	if got := renderSource(t, spec.Spec); got != "reader.yaml" {
		t.Errorf("reader renders %q, want reader.yaml", got)
	}
}

// The render-config script writes the selected template to config.yaml with every key filled in.
func TestRenderConfigInitRendersSelectedTemplate(t *testing.T) {
	const master, groq = "synthetic-master-0123abcd", "gsk_synthetic_4567efgh"
	keys := map[string]string{"__MASTER_API_KEY__": master, "__GROQ_API_KEY__": groq,
		"__OPENAI_API_KEY__": "sk-synthetic", "__GOOGLE_API_KEY__": "AIza-synthetic", "__ANTHROPIC_API_KEY__": "sk-ant-synthetic",
		"__BROKER_PASSWORD__": "synthetic-broker-89ab", "__ANALYTICS_API_KEY__": "synthetic-analytics-cdef"}
	for _, oc := range []*oramav1alpha1.OramaCloudV1{engineOC(nil), engineOC(withSideImages)} {
		h := newEngineHarness(t)
		h.reconcile(oc)
		cm := h.engineConfig()
		for _, spec := range []corev1.PodSpec{h.writer().Spec.Template.Spec, h.reader().Spec.Template.Spec} {
			key := renderSource(t, spec)
			dir := t.TempDir()
			tmpl, out := filepath.Join(dir, "tmpl"), filepath.Join(dir, "etc")
			for _, d := range []string{tmpl, out} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for k, v := range cm.Data {
				if err := os.WriteFile(filepath.Join(tmpl, k), []byte(v), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			script := renderInit(t, spec).Command[2]
			script = strings.ReplaceAll(script, "/tmpl/", tmpl+"/")
			script = strings.ReplaceAll(script, "/etc/oramacore/", out+"/")
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = os.Environ()
			var pairs []string
			for placeholder, v := range keys {
				cmd.Env = append(cmd.Env, strings.Trim(placeholder, "_")+"="+v)
				pairs = append(pairs, placeholder, v)
			}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("render-config: %v: %s", err, b)
			}
			got, err := os.ReadFile(filepath.Join(out, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			want := strings.NewReplacer(pairs...).Replace(cm.Data[key])
			// The combined reader's local writer key is generated by the script itself (#795), whatever
			// the environment holds: 48 hex characters in place of its placeholder.
			if strings.Contains(want, "__LOCAL_WRITER_KEY__") {
				m := regexp.MustCompile(`master_api_key: '([0-9a-f]{48})'`).FindSubmatch(got)
				if m == nil {
					t.Fatalf("rendered %s has no generated local writer key:\n%s", key, got)
				}
				want = strings.Replace(want, "__LOCAL_WRITER_KEY__", string(m[1]), 1)
			}
			if string(got) != want {
				t.Errorf("rendered %s =\n%s\nwant\n%s", key, got, want)
			}
		}
		for k, v := range cm.Data {
			if strings.Contains(v, master) || strings.Contains(v, groq) || strings.Contains(v, "synthetic") {
				t.Errorf("ConfigMap key %s holds a key value", k)
			}
		}
	}
}
