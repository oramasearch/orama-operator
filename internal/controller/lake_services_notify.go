package controller

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	// lakeServicesChannel is notified when lake creates or deletes a managed service.
	lakeServicesChannel = "lake_services"
	// lakeBuildsChannel is notified when lake queues or cancels an in-cluster build.
	lakeBuildsChannel = "lake_builds"
)

// LakeServicesListener wakes the Lake reconciler the moment lake writes a service or build row,
// instead of on the next poll. It keeps one LISTEN connection to each enabled Lake's control
// Postgres and sends that Lake on Events for every notification. The poll stays as the recovery
// path for a missed notification, reconnect, or restart.
type LakeServicesListener struct {
	Client client.Client
	Events chan event.GenericEvent
	// Listen blocks, calling notify for every notification, until ctx ends or the connection
	// fails. Injected for tests; nil means pgxListen.
	Listen func(ctx context.Context, uri string, notify func()) error
	// Resync is how often the set of Lakes is re-read; zero means 10s.
	Resync time.Duration
	// enabled and channel let apps reuse the same leader-only reconnecting listener.
	enabled func(*oramav1alpha1.Lake) bool
	channel string

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// NewLakeServicesListener returns a listener whose Events the Lake controller watches.
func NewLakeServicesListener(c client.Client) *LakeServicesListener {
	return &LakeServicesListener{Client: c, Events: make(chan event.GenericEvent, 64)}
}

func NewLakeAppsListener(c client.Client) *LakeServicesListener {
	return &LakeServicesListener{
		Client: c, Events: make(chan event.GenericEvent, 64), enabled: lakeAppsEnabled, channel: "lake_apps",
	}
}

func NewLakeWorkspacesListener(c client.Client) *LakeServicesListener {
	return &LakeServicesListener{
		Client: c, Events: make(chan event.GenericEvent, 64), enabled: lakeWorkspacesEnabled, channel: "lake_workspaces",
	}
}

// Start runs until ctx ends (a manager Runnable).
func (l *LakeServicesListener) Start(ctx context.Context) error {
	resync := l.Resync
	if resync <= 0 {
		resync = 10 * time.Second
	}
	l.mu.Lock()
	l.running = map[string]context.CancelFunc{}
	l.mu.Unlock()
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		l.sync(ctx)
		select {
		case <-ctx.Done():
			l.mu.Lock()
			for _, cancel := range l.running {
				cancel()
			}
			l.mu.Unlock()
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection: only the leader reconciles, so only the leader listens.
func (l *LakeServicesListener) NeedLeaderElection() bool {
	return true
}

// sync starts a listener for each services-enabled Lake that has none and stops those whose Lake
// is gone or has services turned off.
func (l *LakeServicesListener) sync(ctx context.Context) {
	var lakes oramav1alpha1.LakeList
	if err := l.Client.List(ctx, &lakes); err != nil {
		logf.FromContext(ctx).Info("services listener: cannot list Lakes", "err", err.Error())
		return
	}
	want := map[string]*oramav1alpha1.Lake{}
	enabled := l.enabled
	if enabled == nil {
		enabled = lakeControlRowsEnabled
	}
	for i := range lakes.Items {
		if enabled(&lakes.Items[i]) {
			want[lakes.Items[i].Name] = &lakes.Items[i]
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, cancel := range l.running {
		if want[name] == nil {
			cancel()
			delete(l.running, name)
		}
	}
	for name, lake := range want {
		if l.running[name] != nil {
			continue
		}
		lakeCtx, cancel := context.WithCancel(ctx)
		l.running[name] = cancel
		go l.listen(lakeCtx, lake.DeepCopy())
	}
}

// listen keeps a LISTEN connection to one Lake's control Postgres until ctx ends, reconnecting
// after a failure.
func (l *LakeServicesListener) listen(ctx context.Context, lake *oramav1alpha1.Lake) {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	listen := l.Listen
	if listen == nil {
		listen = func(ctx context.Context, uri string, notify func()) error {
			channel := l.channel
			if channel == "" {
				channel = lakeServicesChannel
			}
			return pgxListenChannel(ctx, uri, channel, notify)
		}
	}
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	notify := func() {
		select {
		case l.Events <- event.GenericEvent{Object: lake}:
		default: // a wake-up is already queued
		}
	}
	for ctx.Err() == nil {
		var app corev1.Secret
		err := l.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lakeControlDB(lake.Name) + "-app"}, &app)
		if err == nil && len(app.Data["uri"]) > 0 {
			err = listen(ctx, string(app.Data["uri"]), notify)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.V(1).Info("Lake desired-state listener reconnecting")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// pgxListen listens to every lake row channel on one Postgres connection, then waits.
func pgxListen(ctx context.Context, uri string, notify func()) error {
	return pgxListenChannels(ctx, uri, []string{lakeServicesChannel, lakeBuildsChannel}, notify)
}

func pgxListenChannel(ctx context.Context, uri, channel string, notify func()) error {
	return pgxListenChannels(ctx, uri, []string{channel}, notify)
}

func pgxListenChannels(ctx context.Context, uri string, channels []string, notify func()) error {
	config, err := pgx.ParseConfig(uri)
	if err != nil {
		return err
	}
	config.ConnectTimeout = serviceStoreConnectTimeout
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["application_name"] = "orama-operator-listen"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, channel := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return err
		}
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		notify()
	}
}
