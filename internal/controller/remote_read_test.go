/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Tests for operator#396: a primary publishes its stream and control DB to read members in other
// clusters, and a read member follows them.

func remoteReadersOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return sideImagesOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.RemoteReaders = &oramav1alpha1.RemoteReadersSpec{AllowedSourceRanges: []string{"52.1.1.1/32", "20.9.9.9/32"}}
		if mut != nil {
			mut(o)
		}
	})
}

// setLoadBalancerIP gives a Service the address a cloud load balancer would.
func setLoadBalancerIP(t *testing.T, cl client.Client, name, ip string) {
	t.Helper()
	svc := getObj(t, cl, name, &corev1.Service{})
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	if err := cl.Status().Update(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
}

// rollBroker reports the broker StatefulSet as rolled out and Ready, as its controller would.
func rollBroker(t *testing.T, cl client.Client) {
	t.Helper()
	sts := getObj(t, cl, "v1-broker", &appsv1.StatefulSet{})
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1,
		CurrentRevision: "r", UpdateRevision: "r"}
	if err := cl.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
}

func getOC(t *testing.T, cl client.Client) *oramav1alpha1.OramaCloudV1 {
	t.Helper()
	return getObj(t, cl, "v1", &oramav1alpha1.OramaCloudV1{})
}

func TestPrimaryPublishesARemoteReadEndpointAndRemovesItWhenOff(t *testing.T) {
	cl, r := reconcileOCV1(t, remoteReadersOC(nil))

	broker := getObj(t, cl, "v1-broker-remote", &corev1.Service{})
	if broker.Spec.Type != corev1.ServiceTypeLoadBalancer || broker.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal ||
		!slices.Equal(broker.Spec.LoadBalancerSourceRanges, []string{"20.9.9.9/32", "52.1.1.1/32"}) {
		t.Fatalf("broker remote Service: type %s, policy %s, ranges %v", broker.Spec.Type, broker.Spec.ExternalTrafficPolicy, broker.Spec.LoadBalancerSourceRanges)
	}
	if len(broker.Spec.Ports) != 1 || broker.Spec.Ports[0].Port != brokerStreamTLSPort || broker.Spec.Selector[componentLabel] != "broker" {
		t.Fatalf("the broker remote Service exposes only the TLS stream port of the broker: %+v, selector %v", broker.Spec.Ports, broker.Spec.Selector)
	}
	db := getObj(t, cl, "v1-controldb-remote", &corev1.Service{})
	if db.Spec.Ports[0].Port != controlDBPort || db.Spec.Selector["cnpg.io/cluster"] != "v1-controldb" || db.Spec.Selector["cnpg.io/instanceRole"] != "primary" ||
		!slices.Equal(db.Spec.LoadBalancerSourceRanges, broker.Spec.LoadBalancerSourceRanges) {
		t.Fatalf("control DB remote Service: ports %+v, selector %v, ranges %v", db.Spec.Ports, db.Spec.Selector, db.Spec.LoadBalancerSourceRanges)
	}

	// The remote readers' account: configure and read on the one stream, nothing else.
	topo := getObj(t, cl, "v1-broker-remote-read", &oramav1alpha1.RabbitMQTopology{})
	if len(topo.Spec.Users) != 1 || topo.Spec.Users[0].Name != remoteReaderBrokerUser || len(topo.Spec.Streams) != 0 {
		t.Fatalf("remote reader topology: %+v", topo.Spec)
	}
	if p := topo.Spec.Permissions[0]; p.Configure != "^oramacore-operations$" || p.Read != "^oramacore-operations$" || p.Write != "^$" {
		t.Fatalf("remote reader permission: %+v", p)
	}

	// No address yet: a CA, no server certificate, no TLS listener, nothing published.
	tlsSecret := getObj(t, cl, "v1-broker-tls", &corev1.Secret{})
	if len(tlsSecret.Data["ca.crt"]) == 0 || len(tlsSecret.Data["tls.crt"]) != 0 {
		t.Fatalf("before the load balancer has an address: keys %v", slices.Sorted(func(yield func(string) bool) {
			for k := range tlsSecret.Data {
				yield(k)
			}
		}))
	}
	if strings.Contains(getObj(t, cl, "v1-broker-config", &corev1.ConfigMap{}).Data["rabbitmq.conf"], "ssl") {
		t.Fatal("the TLS listener needs the server certificate")
	}
	oc := getOC(t, cl)
	if oc.Status.RemoteRead != nil || apimeta.IsStatusConditionTrue(oc.Status.Conditions, remoteReadConditionType) {
		t.Fatalf("nothing is published yet: %+v", oc.Status.RemoteRead)
	}

	// The cloud assigns addresses and CNPG has its Secrets.
	setLoadBalancerIP(t, cl, "v1-broker-remote", "20.30.40.50")
	setLoadBalancerIP(t, cl, "v1-controldb-remote", "20.30.40.60")
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "v1-controldb-ca", Namespace: "orama-system"}, Data: map[string][]byte{"ca.crt": []byte("pg-ca")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "v1-controldb-replication", Namespace: "orama-system"}, Data: map[string][]byte{"tls.crt": []byte("repl-crt"), "tls.key": []byte("repl-key")}},
	} {
		if err := cl.Create(context.Background(), sec); err != nil {
			t.Fatal(err)
		}
	}
	reconcileAgain(t, cl, r, nil)

	tlsSecret = getObj(t, cl, "v1-broker-tls", &corev1.Secret{})
	block, _ := pem.Decode(tlsSecret.Data["tls.crt"])
	if block == nil {
		t.Fatal("no server certificate once the load balancer has an address")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(tlsSecret.Data["ca.crt"])
	for _, name := range []string{"20.30.40.50", "v1-broker.orama-system.svc"} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			t.Fatalf("a client trusting the broker CA cannot verify %s: %v", name, err)
		}
	}

	// The broker step ran before the certificate was issued; the Secret's change reconciles again.
	reconcileAgain(t, cl, r, nil)
	conf := getObj(t, cl, "v1-broker-config", &corev1.ConfigMap{}).Data["rabbitmq.conf"]
	if !strings.Contains(conf, "stream.listeners.ssl.default = 5551\n") || !strings.Contains(conf, "ssl_options.certfile = /etc/rabbitmq/tls/tls.crt\n") ||
		!strings.Contains(conf, "stream.listeners.tcp.1 = 5552\n") {
		t.Fatalf("rabbitmq.conf:\n%s", conf)
	}
	sts := getObj(t, cl, "v1-broker", &appsv1.StatefulSet{})
	if sts.Spec.Template.Annotations[brokerTLSCertAnnotation] != tlsMaterialHash(tlsSecret) {
		t.Fatal("the broker does not restart when its TLS material changes")
	}
	var tlsVolume *corev1.Volume
	for i, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "stream-tls" {
			tlsVolume = &sts.Spec.Template.Spec.Volumes[i]
		}
	}
	if tlsVolume == nil || slices.ContainsFunc(tlsVolume.Secret.Items, func(k corev1.KeyToPath) bool { return k.Key == "ca.key" }) {
		t.Fatalf("the broker mounts its certificate and key, never the CA key: %+v", tlsVolume)
	}
	if !slices.ContainsFunc(sts.Spec.Template.Spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.MountPath == "/etc/rabbitmq/tls/tls.key" && m.SubPath == "tls.key"
	}) {
		t.Fatal("the broker's key is not mounted where rabbitmq.conf names it")
	}

	handoff := getObj(t, cl, "v1-remote-read", &corev1.Secret{})
	if missing := missingKeys(handoff, remoteReadKeys(true)); len(missing) > 0 {
		t.Fatalf("the credentials Secret is missing %v", missing)
	}
	if string(handoff.Data[remoteReadControlDBKeyKey]) != "repl-key" || string(handoff.Data[remoteReadBrokerCAKey]) != string(tlsSecret.Data["ca.crt"]) {
		t.Fatal("the credentials Secret does not carry its sources' values")
	}
	if getOC(t, cl).Status.RemoteRead != nil {
		t.Fatal("published before the broker restarted with its TLS listener")
	}

	rollBroker(t, cl)
	reconcileAgain(t, cl, r, nil)
	oc = getOC(t, cl)
	want := oramav1alpha1.RemoteReadStatus{BrokerStreamEndpoint: "20.30.40.50:5551", ControlDBEndpoint: "20.30.40.60:5432", CredentialsSecret: "v1-remote-read"}
	if oc.Status.RemoteRead == nil || *oc.Status.RemoteRead != want || !apimeta.IsStatusConditionTrue(oc.Status.Conditions, remoteReadConditionType) {
		t.Fatalf("status.remoteRead %+v, conditions %+v", oc.Status.RemoteRead, oc.Status.Conditions)
	}
	if i := slices.IndexFunc(oc.Status.Components, func(c oramav1alpha1.ComponentStatus) bool { return c.Name == remoteReadComponent }); i < 0 || !oc.Status.Components[i].Ready {
		t.Fatalf("components: %+v", oc.Status.Components)
	}

	// Steady: the certificate is not reissued.
	tlsVersion := tlsSecret.ResourceVersion
	reconcileAgain(t, cl, r, nil)
	if getObj(t, cl, "v1-broker-tls", &corev1.Secret{}).ResourceVersion != tlsVersion {
		t.Fatal("a steady primary rewrote its TLS Secret")
	}

	// Off: everything goes, and the broker drops its TLS listener.
	reconcileAgain(t, cl, r, func(o *oramav1alpha1.OramaCloudV1) { o.Spec.RemoteReaders = nil })
	for name, obj := range map[string]client.Object{
		"v1-broker-remote": &corev1.Service{}, "v1-controldb-remote": &corev1.Service{}, "v1-broker-tls": &corev1.Secret{},
		"v1-broker-remote-reader": &corev1.Secret{}, "v1-remote-read": &corev1.Secret{}, "v1-broker-remote-read": &oramav1alpha1.RabbitMQTopology{},
	} {
		if !absent(t, cl, name, obj) {
			t.Errorf("%T %s is left after remoteReaders was removed", obj, name)
		}
	}
	if strings.Contains(getObj(t, cl, "v1-broker-config", &corev1.ConfigMap{}).Data["rabbitmq.conf"], "ssl") {
		t.Fatal("the broker keeps its TLS listener")
	}
	oc = getOC(t, cl)
	if oc.Status.RemoteRead != nil || apimeta.FindStatusCondition(oc.Status.Conditions, remoteReadConditionType) != nil {
		t.Fatalf("status still names the remote endpoint: %+v", oc.Status.RemoteRead)
	}
}

func TestRemoteReadersNeedTheBrokerInThisCluster(t *testing.T) {
	oc := remoteReadersOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Components.Broker.Mode = "external" })
	r := &OramaCloudV1Reconciler{Scheme: ocv1TestScheme(t)}
	if err := r.reconcileRemoteRead(context.Background(), oc); err == nil || !strings.Contains(err.Error(), "the broker must run in this cluster") {
		t.Fatalf("want remoteReaders refused without an in-cluster broker, got %v", err)
	}
}

// readMemberOC is a read member following v1-eu, with a replica control DB.
func readMemberOC(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role = "read"
		o.Spec.Images.Overrides = map[string]string{"oramacore-writer": testWriterImage, "oramacore-reader": testReaderImage}
		c := &o.Spec.Components
		c.Engine.Enabled, c.Engine.Envoy = true, true
		c.ControlDB.Enabled, c.ControlDB.Replicate = true, true
		o.Spec.Peers = []oramav1alpha1.PeerSpec{{Name: "v1-eu", Region: "swedencentral", BrokerStreamEndpoint: "20.30.40.50:5551",
			PostgresEndpoint: "20.30.40.60:5432", CredentialsSecretRef: &corev1.LocalObjectReference{Name: "v1-eu-remote-read"}}}
		if mut != nil {
			mut(o)
		}
	})
}

func peerCredentials() *corev1.Secret {
	data := map[string][]byte{}
	for _, k := range remoteReadKeys(true) {
		data[k] = []byte("value-of-" + k)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "v1-eu-remote-read", Namespace: "orama-system"}, Data: data}
}

func TestReadMemberReadersFollowThePrimarysStreamOverTLS(t *testing.T) {
	cl, _ := reconcileOCV1(t, readMemberOC(nil), peerCredentials())

	if !absent(t, cl, "v1-writer", &appsv1.StatefulSet{}) || !absent(t, cl, "v1-master-api-key", &corev1.Secret{}) {
		t.Fatal("a read member runs no writer and mints no master API key")
	}
	cfg := parseEngineConfig(t, readerConfigKey, getObj(t, cl, "v1-engine-config", &corev1.ConfigMap{}).Data[readerConfigKey])
	input := cfg["reader_side"].(map[string]any)["input"].(map[string]any)
	tls, _ := input["tls"].(map[string]any)
	if input["host"] != "20.30.40.50" || input["port"] != float64(5551) || tls["enabled"] != true || tls["root_certificates_path"] != readerBrokerCAPath {
		t.Fatalf("reader input: %+v", input)
	}

	sts := getObj(t, cl, "v1-reader", &appsv1.StatefulSet{})
	pod := sts.Spec.Template.Spec
	if wait := pod.InitContainers[0]; wait.Name != "wait-for-broker" || !strings.Contains(wait.Command[2], "nc -z 20.30.40.50 5551;") {
		t.Fatalf("the readers wait on the primary's stream: %v", wait.Command)
	}
	render := renderInit(t, sts)
	assertEnvFromSecret(t, "reader render-config", render.Env, brokerUserEnv, "v1-eu-remote-read/"+remoteReadBrokerUserKey, -1)
	assertEnvFromSecret(t, "reader render-config", render.Env, brokerPasswordEnv, "v1-eu-remote-read/"+remoteReadBrokerPasswordKey, -1)
	assertEnvFromSecret(t, "reader render-config", render.Env, masterAPIKeyKey, "v1-eu-remote-read/"+masterAPIKeyKey, -1)
	if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
		return v.Secret != nil && v.Secret.SecretName == "v1-eu-remote-read" && len(v.Secret.Items) == 1 && v.Secret.Items[0].Key == remoteReadBrokerCAKey
	}) || !slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == readerBrokerCADir }) {
		t.Fatal("the readers do not mount the primary's broker CA (and only it)")
	}

	oc := getOC(t, cl)
	if !apimeta.IsStatusConditionTrue(oc.Status.Conditions, peerConditionType) {
		t.Fatalf("conditions: %+v", oc.Status.Conditions)
	}
}

// The replica control DB copies the primary with pg_basebackup and streams from it, authenticated by
// the primary's streaming_replica certificate and verifying the primary against its CA.
func TestReadMemberReplicaControlDBStreamsFromThePrimary(t *testing.T) {
	cl, _ := reconcileOCV1(t, readMemberOC(nil), peerCredentials())
	spec := controlDBCluster(t, cl).Object["spec"].(map[string]any)

	if source, _, _ := unstructured.NestedString(spec, "bootstrap", "pg_basebackup", "source"); source != "primary" {
		t.Fatalf("bootstrap: %+v", spec["bootstrap"])
	}
	if enabled, _, _ := unstructured.NestedBool(spec, "replica", "enabled"); !enabled {
		t.Fatalf("replica: %+v", spec["replica"])
	}
	external := spec["externalClusters"].([]any)[0].(map[string]any)
	params := external["connectionParameters"].(map[string]any)
	if params["host"] != "20.30.40.60" || params["port"] != "5432" || params["user"] != "streaming_replica" ||
		params["dbname"] != "postgres" || params["sslmode"] != "verify-ca" {
		t.Fatalf("connection parameters: %+v", params)
	}
	for field, key := range map[string]string{"sslCert": remoteReadControlDBCertKey, "sslKey": remoteReadControlDBKeyKey, "sslRootCert": remoteReadControlDBCAKey} {
		ref := external[field].(map[string]any)
		if ref["name"] != "v1-eu-remote-read" || ref["key"] != key {
			t.Errorf("%s: %+v", field, ref)
		}
	}
}

// A read member without a usable peer starts no readers and no replica, and says why.
func TestReadMemberWithoutAUsablePeerWaits(t *testing.T) {
	for name, tc := range map[string]struct {
		oc      *oramav1alpha1.OramaCloudV1
		objs    []client.Object
		message string
	}{
		"no peer":             {readMemberOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Peers = nil }), nil, "spec.peers names no primary"},
		"no Secret":           {readMemberOC(nil), nil, "Secret v1-eu-remote-read (peer v1-eu's remote-read credentials) does not exist"},
		"no postgresEndpoint": {readMemberOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Peers[0].PostgresEndpoint = "" }), []client.Object{peerCredentials()}, "has no postgresEndpoint"},
		"incomplete Secret": {readMemberOC(nil), []client.Object{func() client.Object {
			s := peerCredentials()
			delete(s.Data, remoteReadControlDBKeyKey)
			return s
		}()}, "is missing controldb-replication.key"},
	} {
		t.Run(name, func(t *testing.T) {
			cl, _ := reconcileOCV1(t, tc.oc, tc.objs...)
			if !absent(t, cl, "v1-reader", &appsv1.StatefulSet{}) {
				t.Fatal("readers started without a stream to follow")
			}
			cluster := &unstructured.Unstructured{}
			cluster.SetGroupVersionKind(cnpgClusterGVK)
			if !absent(t, cl, "v1-controldb", cluster) {
				t.Fatal("a replica was created without a primary to copy")
			}
			oc := getOC(t, cl)
			condition := apimeta.FindStatusCondition(oc.Status.Conditions, peerConditionType)
			if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, tc.message) || oc.Status.Phase == "Ready" {
				t.Fatalf("phase %q, PeerReady %+v", oc.Status.Phase, condition)
			}
		})
	}
}

func TestReadMemberNeedsSideOnlyEngineImages(t *testing.T) {
	oc := readMemberOC(func(o *oramav1alpha1.OramaCloudV1) { o.Spec.Images.Overrides = nil })
	r := &OramaCloudV1Reconciler{Scheme: ocv1TestScheme(t)}
	if err := r.reconcileEngine(context.Background(), oc); err == nil || !strings.Contains(err.Error(), "side-only engine images") {
		t.Fatalf("want the combined image refused on a read member, got %v", err)
	}
}

// A primary switched to read stops its writer, and its control DB, which has its own history, is not
// turned into a replica of another.
func TestAPrimarySwitchedToReadStopsItsWriterAndKeepsItsControlDB(t *testing.T) {
	cl, r := reconcileOCV1(t, readMemberOC(func(o *oramav1alpha1.OramaCloudV1) {
		o.Spec.Region.Role, o.Spec.Peers = "primary", nil
	}))
	getObj(t, cl, "v1-writer", &appsv1.StatefulSet{})
	if err := cl.Create(context.Background(), peerCredentials()); err != nil {
		t.Fatal(err)
	}
	read := readMemberOC(nil)
	oc := getOC(t, cl)
	oc.Spec.Region.Role, oc.Spec.Peers = "read", read.Spec.Peers
	if err := cl.Update(context.Background(), oc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(oc)}); err == nil ||
		!strings.Contains(err.Error(), "was created as a primary and cannot become a replica") {
		t.Fatalf("want the control DB kept as it is, got %v", err)
	}
	if !absent(t, cl, "v1-writer", &appsv1.StatefulSet{}) {
		t.Fatal("a read member still runs a writer")
	}
	if _, found, _ := unstructured.NestedMap(controlDBCluster(t, cl).Object, "spec", "bootstrap", "initdb"); !found {
		t.Fatal("the primary's control DB was rewritten")
	}
}
