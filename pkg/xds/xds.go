package xds

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	discovery_v3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/uber/jaeger-client-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"sigs.k8s.io/yaml"
)

var (
	// A timestamp indiciating when we last generated a new config and began pushing it to clients.
	xdsConfigLastUpdated = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ekglue_xds_config_last_updated",
		Help: "A timestamp indicating when we last generated a new config and began pushing it to clients.",
	}, []string{"manager_name", "config_type"})

	// A history of acceptance/rejection of every config version generated by this process.
	xdsConfigAcceptanceStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ekglue_xds_config_acceptance_status",
		Help: "The number of Envoy instances that have accepted or rejected a config.",
	}, []string{"manager_name", "config_type", "status"})

	// A count of how many times a given resource has been pushed.
	xdsResourcePushCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ekglue_xds_resource_push_count",
		Help: "The number of times a named resource has been pushed.",
	}, []string{"manager_name", "config_type", "resource_name"})

	// A timestamp of when each resource was last pushed.
	xdsResourcePushAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ekglue_xds_resource_push_age",
		Help: "The time when the named resource was last pushed.",
	}, []string{"manager_name", "config_type", "resource_name"})
)

// Resource is an xDS resource, like envoy_api_v2.Cluster, etc.
type Resource interface {
	proto.Message
	Validate() error
}

func resourceName(r Resource) string {
	if x, ok := r.(interface{ GetName() string }); ok {
		return x.GetName()
	}
	if x, ok := r.(interface{ GetClusterName() string }); ok {
		return x.GetClusterName()
	}
	panic(fmt.Sprintf("unable to name resource %v", r))
}

// Update is information about a resource change.
type update struct {
	span      opentracing.Span
	resources map[string]struct{} // set of resources that changed; must not be written to
}

// Session is a channel that receives notifications when the managed resources change.
type session chan update

// Acknowledgment is an event that represents the client accepting or rejecting a configuration.
type Acknowledgment struct {
	Node    string // The id of the node.
	Version string // The full version.
	Ack     bool   // Whether this is an ack or nack.
}

// Manager consumes a stream of resource change, and notifies connected xDS clients of the change.
// It is not safe to mutate any public fields after the manager has received a client connection.
type Manager struct {
	// Name is the name of this manager, for logging/monitoring.
	Name string
	// VersionPrefix is a prefix to prepend to the version number, typically the server's pod name.
	VersionPrefix string
	// Type is the type of xDS resource being managed, like "type.googleapis.com/envoy.api.v2.Cluster".
	Type string
	// OnAck is a function that will be called when a config is accepted or rejected.
	OnAck func(Acknowledgment)
	// Logger is a zap logger to use to log manager events.  Per-connection events are logged
	// via the logger stored in the request context.
	Logger *zap.Logger
	// Draining is a channel that, when closed, will drain client connections.
	Draining chan struct{}

	resourcesMu sync.Mutex
	resources   map[string]Resource
	version     int

	sessionsMu sync.Mutex
	sessions   map[session]struct{}
}

// NewManager creates a new manager.  resource is an instance of the type to manage.
func NewManager(name, versionPrefix string, resource Resource, drainCh chan struct{}) *Manager {
	m := &Manager{
		Name:          name,
		VersionPrefix: versionPrefix,
		Type:          "type.googleapis.com/" + string(resource.ProtoReflect().Descriptor().FullName()),
		Logger:        zap.L().Named(name),
		Draining:      drainCh,
		resources:     make(map[string]Resource),
		sessions:      make(map[session]struct{}),
	}
	return m
}

// version returns the version number of the current config.  You must hold the resource lock.
func (m *Manager) versionString() string {
	return fmt.Sprintf("%s%d", m.VersionPrefix, m.version)
}

// snapshotAll returns the current list of managed resources.  You must hold the resource lock.
func (m *Manager) snapshotAll() ([]*anypb.Any, []string, string, error) {
	result := make([]*anypb.Any, 0, len(m.resources))
	names := make([]string, 0, len(m.resources))
	for n, r := range m.resources {
		any, err := anypb.New(r)
		if err != nil {
			return nil, nil, "", fmt.Errorf("marshal resource %s to any: %w", n, err)
		}
		names = append(names, n)
		result = append(result, any)
	}
	return result, names, m.versionString(), nil
}

// snapshot returns a subset of managed resources.  You must hold the Manager's lock.
func (m *Manager) snapshot(want []string) ([]*anypb.Any, []string, string, error) {
	if len(want) == 0 {
		return m.snapshotAll()
	}
	result := make([]*anypb.Any, 0, len(want))
	names := make([]string, 0, len(want))
	for _, name := range want {
		r, ok := m.resources[name]
		if !ok {
			// NOTE(jrockway): Because discovery is "eventually consistent", this is OK.
			// A service might exist without any endpoints, so when Envoy loads that
			// cluster it will subscribe to those endpoints, there just won't be any
			// yet.  When an endpoint shows up, then it will be sent.  As a result, this
			// log message might be too spammy, but we'll see.
			m.Logger.Debug("requested resource is not available", zap.String("resource_name", name))
			continue
		}
		any, err := anypb.New(r)
		if err != nil {
			return nil, nil, "", fmt.Errorf("marshal resource %s to any: %w", name, err)
		}
		names = append(names, name)
		result = append(result, any)
	}
	// TODO(jrockway): Return a better version string, probably max(resource[].version) (which
	// we don't track right now, but is available in the k8s api objects).
	return result, names, m.versionString(), nil
}

// notify notifies connected clients of the change.
func (m *Manager) notify(ctx context.Context, resources []string) error {
	if len(resources) < 1 {
		return nil
	}
	m.resourcesMu.Lock()
	m.version++
	m.resourcesMu.Unlock()
	xdsConfigLastUpdated.WithLabelValues(m.Name, m.Type).SetToCurrentTime()

	u := update{span: opentracing.SpanFromContext(ctx), resources: make(map[string]struct{})}
	for _, name := range resources {
		u.resources[name] = struct{}{}
	}

	m.Logger.Debug("new resource version", zap.Int("version", m.version), zap.Strings("resources", resources))

	// First try sending to sessions that aren't busy.
	blocked := make(map[session]struct{})
	m.sessionsMu.Lock()
	for session := range m.sessions {
		select {
		case session <- u:
		default:
			blocked[session] = struct{}{}
		}
	}
	// Then use the context to wait on busy sessions.
	for len(blocked) > 0 {
		for session := range blocked {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case session <- u:
				delete(blocked, session)
			case <-timer.C: // Don't spend the whole time interval on one slow session.
			case <-ctx.Done():
				m.Logger.Error("change notification timed out", zap.Int("sessions_missed", len(blocked)))
				return ctx.Err()
			}
		}
	}
	m.sessionsMu.Unlock()
	return nil
}

// Add adds or replaces (by name) managed resources, and notifies connected clients of the change.
func (m *Manager) Add(ctx context.Context, rs []Resource) error {
	var changed []string
	for _, r := range rs {
		n := resourceName(r)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%q: %w", n, err)
		}
		m.resourcesMu.Lock()
		if _, overwrote := m.resources[n]; overwrote {
			// TODO(jrockway): Check that this resource actually changed.
			m.Logger.Info("resource updated", zap.String("name", n))
		} else {
			m.Logger.Info("resource added", zap.String("name", n))
		}
		changed = append(changed, n)
		m.resources[n] = r
		m.resourcesMu.Unlock()
	}
	m.notify(ctx, changed)
	return nil
}

// Replace repaces the entire set of managed resources with the provided argument, and notifies
// connected clients of the change.
func (m *Manager) Replace(ctx context.Context, rs []Resource) error {
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%q: %w", resourceName(r), err)
		}
	}
	m.resourcesMu.Lock()
	var changed []string
	old := m.resources
	m.resources = make(map[string]Resource)
	for _, r := range rs {
		n := resourceName(r)
		if _, overwrote := old[n]; overwrote {
			m.Logger.Info("resource updated", zap.String("name", n))
			delete(old, n)
		} else {
			m.Logger.Info("resource added", zap.String("name", n))
		}
		changed = append(changed, n)
		m.resources[n] = r
	}
	for n := range old {
		changed = append(changed, n)
		m.Logger.Info("resource deleted", zap.String("name", n))
	}
	m.resourcesMu.Unlock()
	m.notify(ctx, changed)
	return nil
}

// Delete deletes a single resource by name and notifies clients of the change.
func (m *Manager) Delete(ctx context.Context, n string) {
	m.resourcesMu.Lock()
	if _, ok := m.resources[n]; ok {
		delete(m.resources, n)
		m.Logger.Info("resource deleted", zap.String("name", n))
		m.resourcesMu.Unlock()
		m.notify(ctx, []string{n})
		return
	}
	m.resourcesMu.Unlock()
}

// ListKeys returns the sorted names of managed resources.
func (m *Manager) ListKeys() []string {
	m.resourcesMu.Lock()
	defer m.resourcesMu.Unlock()
	result := make([]string, 0, len(m.resources))
	for _, r := range m.resources {
		result = append(result, resourceName(r))
	}
	sort.Strings(result)
	return result
}

// List returns the managed resources.
func (m *Manager) List() []Resource {
	m.resourcesMu.Lock()
	defer m.resourcesMu.Unlock()
	result := make([]Resource, 0, len(m.resources))
	for _, r := range m.resources {
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool {
		return resourceName(result[i]) < resourceName(result[j])
	})
	return result
}

type tx struct {
	start   time.Time
	span    opentracing.Span
	nonce   string
	version string
}

type loggableSpan struct{ opentracing.Span }

func (t *tx) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if t == nil {
		return errors.New("nil tx")
	}
	enc.AddDuration("age", time.Since(t.start))
	enc.AddString("nonce", t.nonce)
	enc.AddString("version", t.version)
	enc.AddObject("trace", &loggableSpan{t.span})
	return nil
}

func (s *loggableSpan) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if s == nil || s.Span == nil {
		return nil
	}

	j, ok := s.Context().(jaeger.SpanContext)
	if ok {
		if !j.IsValid() {
			return fmt.Errorf("invalid span: %v", j.SpanID())
		}
		enc.AddString("span", j.SpanID().String())
		enc.AddBool("sampled", j.IsSampled())
		return nil
	}

	c := make(opentracing.TextMapCarrier)
	if err := s.Tracer().Inject(s.Context(), opentracing.TextMap, c); err != nil {
		return err
	}
	for k, v := range c {
		enc.AddString(k, v)
	}
	return nil
}

func randomString() string {
	hash := [8]byte{'x', 'x', 'x', 'x', 'x', 'x', 'x', 'x'}
	if n, err := rand.Read(hash[0:8]); n >= 8 && err == nil {
		for i := 0; i < len(hash); i++ {
			hash[i] = hash[i]%26 + 'a'
		}
	}
	return string(hash[0:8])
}

func (m *Manager) BuildDiscoveryResponse(subscribed []string) (*discovery_v3.DiscoveryResponse, []string, error) {
	m.resourcesMu.Lock()
	defer m.resourcesMu.Unlock()
	resources, names, version, err := m.snapshot(subscribed)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot resources: %w", err)
	}
	hash := randomString()
	res := &discovery_v3.DiscoveryResponse{
		VersionInfo: version,
		TypeUrl:     m.Type,
		Resources:   resources,
		Nonce:       fmt.Sprintf("nonce-%s-%s", version, hash),
	}
	if err := res.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate generated discovery response: %w", err)
	}
	return res, names, nil
}

// Stream manages a client connection.  Requests from the client are read from reqCh, responses are
// written to resCh, and the function returns when no further progress can be made.
func (m *Manager) Stream(ctx context.Context, reqCh chan *discovery_v3.DiscoveryRequest, resCh chan *discovery_v3.DiscoveryResponse) error {
	l := ctxzap.Extract(ctx).With(zap.String("xds_type", m.Type))

	// Channel for receiving resource updates.
	rCh := make(session)
	m.sessionsMu.Lock()
	m.sessions[rCh] = struct{}{}
	m.sessionsMu.Unlock()

	// In-flight transactions.
	txs := map[string]*tx{}

	// Cleanup.
	defer func() {
		go func() {
			// Keep the channel drained while we're waiting for the sessionsMu.
			for {
				_, ok := <-rCh
				if !ok {
					return
				}
			}
		}()
		m.sessionsMu.Lock()
		delete(m.sessions, rCh)
		close(rCh)
		m.sessionsMu.Unlock()
		for _, t := range txs {
			t.span.Finish()
		}
	}()

	// Node name arrives in the first request, and is used for all subsequent operations.
	var node string

	// Resources that the client is interested in
	var resources []string

	// sendUpdate starts a new transaction and sends the current resource list.
	sendUpdate := func(ctx context.Context) error {
		span, ctx := opentracing.StartSpanFromContext(ctx, "xds.push", ext.SpanKindConsumer)
		t := &tx{start: time.Now(), span: span}

		buildSpan := opentracing.StartSpan("xds.build_response", opentracing.ChildOf(span.Context()))
		res, names, err := m.BuildDiscoveryResponse(resources)
		buildSpan.Finish()
		if err != nil {
			l.Error("problem building response", zap.Error(err))
			return fmt.Errorf("problem building response: %w", err)
		}
		ext.PeerService.Set(span, node)
		span.SetTag("xds_type", m.Type)
		span.SetTag("xds_version", res.GetVersionInfo())
		resourceTag := fmt.Sprintf("%d total: %s", len(names), strings.Join(names, ","))
		if len(resourceTag) > 64 {
			resourceTag = fmt.Sprintf("%s...", resourceTag[0:61])
		}
		span.SetTag("xds_resources", resourceTag)
		t.version = res.GetVersionInfo()
		t.nonce = res.GetNonce()
		l.Info("pushing updated resources", zap.Object("tx", t), zap.Strings("resources", names))

		select {
		case resCh <- res:
			for _, n := range names {
				xdsResourcePushCount.WithLabelValues(m.Name, m.Type, n).Inc()
				xdsResourcePushAge.WithLabelValues(m.Name, m.Type, n).SetToCurrentTime()
			}
			txs[res.GetNonce()] = t
			span.LogEvent("pushed resources")
			return nil
		case <-ctx.Done():
			err := ctx.Err()
			l.Info("push timed out", zap.Object("tx", t), zap.Error(err))
			ext.LogError(span, fmt.Errorf("push timed out: %w", err))
			t.span.Finish()
			return fmt.Errorf("push timed out: %v", err)
		}
	}

	// handleTx handles an acknowledgement
	handleTx := func(t *tx, req *discovery_v3.DiscoveryRequest) {
		t.span.LogEvent("got response")
		var ack bool
		origVersion, version := t.version, req.GetVersionInfo()
		if err := req.GetErrorDetail(); err != nil {
			ext.LogError(t.span, fmt.Errorf("envoy rejected configuration: %v", err.GetMessage()))
			l.Error("envoy rejected configuration", zap.Any("error", err), zap.String("version.rejected", origVersion), zap.String("version.in_use", version), zap.Object("tx", t))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, "NACK").Inc()
		} else {
			ack = true
			l.Info("envoy accepted configuration", zap.String("version.in_use", version), zap.String("version.sent", origVersion), zap.Object("tx", t))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, "ACK").Inc()
			if version != origVersion {
				l.Warn("envoy acknowledged a config version that does not correspond to what we sent", zap.String("version.in_use", version), zap.String("version.sent", origVersion), zap.Object("tx", t))
			}
		}
		status := "NACK"
		if ack {
			status = "ACK"
		}
		t.span.SetTag("status", status)

		if f := m.OnAck; f != nil {
			f(Acknowledgment{
				Ack:     ack,
				Node:    node,
				Version: version,
			})
		}
		t.span.Finish()
		delete(txs, t.nonce)
	}

	// when cleanupTicker ticks, we attempt to delete transactions that have been forgotten.
	cleanupTicker := time.NewTicker(time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-m.Draining:
			return errors.New("server draining")
		case <-ctx.Done():
			return ctx.Err()
		case <-cleanupTicker.C:
			for key, t := range txs {
				if time.Since(t.start) > time.Minute {
					l.Debug("cleaning up stale transaction", zap.Object("tx", t))
					ext.LogError(t.span, errors.New("transaction went stale"))
					t.span.Finish()
					delete(txs, key)
				}
			}
		case req, ok := <-reqCh:
			if !ok {
				return errors.New("request channel closed")
			}
			newResources := req.GetResourceNames()
			if node == "" {
				node = req.GetNode().GetId()
				l = l.With(zap.String("envoy.node.id", node))
				ctx = ctxzap.ToContext(ctx, l)
				resources = newResources
				l = l.With(zap.Strings("subscribed_resources", resources))
			}
			if diff := cmp.Diff(resources, newResources); diff != "" {
				// I am pretty sure xDS doesn't allow changing the subscribed
				// resource set, so we warn about attempting to do so.  I guess if
				// we see this warning, it means that being "pretty sure" was
				// incorrect.
				l.Warn("envoy changed resource subscriptions without opening a new stream", zap.Strings("new_resources", newResources))
				return status.Error(codes.FailedPrecondition, "resource subscriptions changed unexpectedly")
			}

			if t := req.GetTypeUrl(); t != m.Type {
				l.Error("ignoring wrong-type discovery request", zap.String("manager_type", m.Type), zap.String("requested_type", t))
				return status.Error(codes.InvalidArgument, "wrong resource type requested")
			}

			nonce := req.GetResponseNonce()
			if t, ok := txs[nonce]; ok {
				handleTx(t, req)
				break
			}
			if nonce == "" {
				l.Info("sending initial config")
			} else {
				// This is not that alarming.  It will happen when ekglue restarts
				// and Envoy connects to a new replica.
				l.Info("envoy sent acknowledgement of unrecognized nonce; resending config", zap.String("nonce", nonce))
			}
			tctx, c := context.WithTimeout(ctx, 5*time.Second)
			if err := sendUpdate(tctx); err != nil {
				c()
				return fmt.Errorf("pushing resources: %w", err)
			}
			c()
		case u := <-rCh:
			var send bool
			for _, name := range resources {
				if _, ok := u.resources[name]; ok {
					send = true
					break
				}
			}
			if len(resources) == 0 || send {
				tctx, c := context.WithTimeout(ctx, 5*time.Second)
				if err := sendUpdate(opentracing.ContextWithSpan(tctx, u.span)); err != nil {
					c()
					return fmt.Errorf("pushing resources: %w", err)
				}
				c()
			}
		}
	}
}

// Stream is the API shared among all envoy_api_v2.[type]DiscoveryService_Stream[type]Server
// streams.
type Stream interface {
	Context() context.Context
	Recv() (*discovery_v3.DiscoveryRequest, error)
	Send(*discovery_v3.DiscoveryResponse) error
}

// StreamGRPC adapts a gRPC stream of DiscoveryRequest -> DiscoveryResponse to the API required by
// the Stream function.
func (m *Manager) StreamGRPC(stream Stream) error {
	ctx := stream.Context()
	l := ctxzap.Extract(ctx)
	reqCh := make(chan *discovery_v3.DiscoveryRequest)
	resCh := make(chan *discovery_v3.DiscoveryResponse)
	errCh := make(chan error)

	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				close(reqCh)
				return
			}
			reqCh <- req
		}
	}()

	go func() {
		for {
			res, ok := <-resCh
			if !ok {
				return
			}
			if err := stream.Send(res); err != nil {
				l.Debug("error writing message to stream", zap.Error(err))
			}
		}
	}()

	go func() { errCh <- m.Stream(ctx, reqCh, resCh) }()
	err := <-errCh
	close(resCh)
	close(errCh)
	return err
}

// ConfigAsYAML dumps the currently-tracked resources as YAML.
func (m *Manager) ConfigAsYAML(verbose bool) ([]byte, error) {
	rs := m.List()
	sort.Slice(rs, func(i, j int) bool {
		return resourceName(rs[i]) < resourceName(rs[j])
	})

	list := struct {
		Resources []json.RawMessage `json:"resources"`
	}{}
	jsonm := &protojson.MarshalOptions{EmitUnpopulated: verbose}
	for _, r := range rs {
		j, err := jsonm.Marshal(r)
		if err != nil {
			return nil, err
		}
		list.Resources = append(list.Resources, []byte(j))
	}
	js, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}

	ya, err := yaml.JSONToYAML([]byte(js))
	if err != nil {
		return nil, err
	}
	return ya, nil
}

// ServeHTTP dumps the currently-tracked resources as YAML.
//
// It will normally omit defaults, but with "?verbose" in the query params, it will print those too.
func (m *Manager) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, verbose := req.URL.Query()["verbose"]
	ya, err := m.ConfigAsYAML(verbose)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(ya)
}
