// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A notification on lake_services wakes the Lake it came from; a Lake without control rows is not
// listened to (lake#197).
func TestServicesListenerWakesTheLakeOnNotify(t *testing.T) {
	scheme := newLakeScheme(t)
	withServices := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: oramav1alpha1.LakeSpec{Namespace: "lake", Services: oramav1alpha1.LakeServicesSpec{}}}
	without := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "quiet"}, Spec: oramav1alpha1.LakeSpec{Namespace: "lake"}}
	app := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB("prod") + "-app", Namespace: "lake"},
		Data: map[string][]byte{"uri": []byte("postgresql://lake@db/lake")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(withServices, without, app).Build()

	listened := make(chan string, 4)
	l := NewLakeServicesListener(c)
	l.Resync = 50 * time.Millisecond
	l.Listen = func(ctx context.Context, uri string, notify func()) error {
		listened <- uri
		notify()
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Start(ctx) }()

	select {
	case uri := <-listened:
		if uri != "postgresql://lake@db/lake" {
			t.Fatalf("listened on %q", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no listener started")
	}
	select {
	case e := <-l.Events:
		if e.Object.GetName() != "prod" {
			t.Fatalf("woke %q, want prod", e.Object.GetName())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no wake-up")
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case uri := <-listened:
		t.Fatalf("a second listener started (%q); one per services-enabled Lake", uri)
	default:
	}
}

func TestBuildListenerRunsWhenServicesAreDisabled(t *testing.T) {
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{ObjectMeta: metav1.ObjectMeta{Name: "builder"},
		Spec: oramav1alpha1.LakeSpec{Namespace: "lake", Builds: oramav1alpha1.LakeBuildsSpec{}}}
	app := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeControlDB("builder") + "-app", Namespace: "lake"},
		Data: map[string][]byte{"uri": []byte("postgresql://lake@build-db/lake")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake, app).Build()
	listened := make(chan string, 1)
	l := NewLakeServicesListener(c)
	l.Resync = time.Hour
	l.Listen = func(ctx context.Context, uri string, notify func()) error {
		listened <- uri
		notify()
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Start(ctx) }()
	select {
	case uri := <-listened:
		if uri != "postgresql://lake@build-db/lake" {
			t.Fatalf("listened on %q", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no build listener started")
	}
	select {
	case event := <-l.Events:
		if event.Object.GetName() != lake.Name {
			t.Fatalf("woke %q, want %s", event.Object.GetName(), lake.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no build wake-up")
	}
}
