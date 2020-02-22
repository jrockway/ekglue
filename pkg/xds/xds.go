package xds

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/google/go-cmp/cmp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/jrockway/opinionated-server/server"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/uber/jaeger-client-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"
)

var (
	// A history of xds versions generated by this process.
	xdsConfigVersions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "xds_config_versions",
		Help: "The age of the named config_version.",
	}, []string{"manager_name", "config_type", "config_version"})

	// A history of acceptance/rejection of every config version generated by this process.
	xdsConfigAcceptanceStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "xds_config_acceptance_status",
		Help: "The number of Envoy instances that have accepted or rejected a config version.",
	}, []string{"manager_name", "config_type", "config_version", "status"})
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

// Session is a channel that receives notifications when the managed resources change.
type session chan struct{}

// Acknowledgment is an event that represents the client accepting or rejecting a configuration.
type Acknowledgment struct {
	Node    string // The id of the node.
	Version string // The full version.
	Ack     bool   // Whether this is an ack or nack.
}

// Manager consumes a stream of resource change, and notifies connected xDS clients of the change.
// It is not safe to mutate any public fields after the manager has received a client connection
// without taking the lock.
type Manager struct {
	sync.Mutex
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

	version   int
	resources map[string]Resource
	sessions  map[session]struct{}
}

// NewManager creates a new manager.  resource is an instance of the type to manage.
func NewManager(name, versionPrefix string, resource Resource) *Manager {
	m := &Manager{
		Name:          name,
		VersionPrefix: versionPrefix,
		Type:          "type.googleapis.com/" + proto.MessageName(resource),
		Logger:        zap.L().Named(name),
		resources:     make(map[string]Resource),
		sessions:      make(map[session]struct{}),
	}
	return m
}

// version returns the version number of the current config.  You must hold the Manager's lock.
func (m *Manager) versionString() string {
	return fmt.Sprintf("%s%d", m.VersionPrefix, m.version)
}

// snapshotAll returns the current list of managed resources.  You must hold the Manager's lock.
func (m *Manager) snapshotAll() ([]*any.Any, string, error) {
	result := make([]*any.Any, 0, len(m.resources))
	for n, r := range m.resources {
		any, err := ptypes.MarshalAny(r)
		if err != nil {
			return nil, "", fmt.Errorf("marshal resource %s to any: %w", n, err)
		}
		result = append(result, any)
	}
	return result, m.versionString(), nil
}

// snapshot returns a subset of managed resources.  You must hold the Manager's lock.
func (m *Manager) snapshot(want []string) ([]*any.Any, string, error) {
	if len(want) == 0 {
		return m.snapshotAll()
	}
	result := make([]*any.Any, 0, len(m.resources))
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
		any, err := ptypes.MarshalAny(r)
		if err != nil {
			return nil, "", fmt.Errorf("marshal resource %s to any: %w", name, err)
		}
		result = append(result, any)
	}
	// TODO(jrockway): Return a better version string, probably max(resource[].version) (which
	// we don't track right now, but is available in the k8s api objects).
	return result, m.versionString(), nil
}

// notify notifies connected clients of the change.  You must hold the Manager's lock.
func (m *Manager) notify() {
	m.version++
	xdsConfigVersions.WithLabelValues(m.Name, m.Type, m.versionString()).SetToCurrentTime()
	var blocked int
	for session := range m.sessions {
		select {
		case session <- struct{}{}:
			m.Logger.Debug("notified session of new version", zap.Int("version", m.version))
		default:
			blocked++
		}
	}
	if blocked > 0 {
		m.Logger.Warn("change notification would have blocked", zap.Int("clients_missed", blocked))
	}
}

// Add adds or replaces (by name) managed resources, and notifies connected clients of the change.
func (m *Manager) Add(rs []Resource) error {
	m.Lock()
	defer m.Unlock()
	for _, r := range rs {
		n := resourceName(r)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%q: %w", n, err)
		}
		if _, overwrote := m.resources[n]; overwrote {
			m.Logger.Info("resource updated", zap.String("name", n))
		} else {
			m.Logger.Info("resource added", zap.String("name", n))
		}
		m.resources[n] = r
	}
	m.notify()
	return nil
}

// Replace repaces the entire set of managed resources with the provided argument, and notifies
// connected clients of the change.
func (m *Manager) Replace(rs []Resource) error {
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%q: %w", resourceName(r), err)
		}
	}
	m.Lock()
	defer m.Unlock()
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
		m.resources[n] = r
	}
	for n := range old {
		m.Logger.Info("resource deleted", zap.String("name", n))
	}
	m.notify()
	return nil
}

// Delete deletes a single resource by name and notifies clients of the change.
func (m *Manager) Delete(n string) {
	m.Lock()
	defer m.Unlock()
	if _, ok := m.resources[n]; ok {
		delete(m.resources, n)
		m.Logger.Info("resource deleted", zap.String("name", n))
	}
	m.notify()
}

// ListKeys returns the sorted names of managed resources.
func (m *Manager) ListKeys() []string {
	m.Lock()
	defer m.Unlock()
	result := make([]string, 0, len(m.resources))
	for _, r := range m.resources {
		result = append(result, resourceName(r))
	}
	sort.Strings(result)
	return result
}

// List returns the managed resources.
func (m *Manager) List() []Resource {
	m.Lock()
	defer m.Unlock()
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

func (m *Manager) BuildDiscoveryResponse(streamID string, subscribed []string) (*envoy_api_v2.DiscoveryResponse, error) {
	m.Lock()
	defer m.Unlock()
	resources, version, err := m.snapshot(subscribed)
	if err != nil {
		return nil, fmt.Errorf("snapshot resources: %w", err)
	}
	hash := randomString()
	res := &envoy_api_v2.DiscoveryResponse{
		VersionInfo: fmt.Sprintf("%s-%s", streamID, version),
		TypeUrl:     m.Type,
		Resources:   resources,
		Nonce:       fmt.Sprintf("nonce-%s-%s", version, hash),
	}
	if err := res.Validate(); err != nil {
		return nil, fmt.Errorf("validate generated discovery response: %w", err)
	}
	return res, nil
}

// Stream manages a client connection.  Requests from the client are read from reqCh, responses are
// written to resCh, and the function returns when no further progress can be made.
func (m *Manager) Stream(ctx context.Context, reqCh chan *envoy_api_v2.DiscoveryRequest, resCh chan *envoy_api_v2.DiscoveryResponse) error {
	l := ctxzap.Extract(ctx).With(zap.String("xds_type", m.Type))

	// Channel for receiving resource updates.
	rCh := make(session, 1)
	m.Lock()
	m.sessions[rCh] = struct{}{}
	m.Unlock()

	// In-flight transactions.
	txs := map[string]*tx{}

	// Cleanup.
	defer func() {
		m.Lock()
		delete(m.sessions, rCh)
		close(rCh)
		m.Unlock()
		for _, t := range txs {
			t.span.Finish()
		}
	}()

	// Node name arrives in the first request, and is used for all subsequent operations.
	var node string

	// Resources that the client is interested in
	var resources []string

	streamID := randomString()
	l = l.With(zap.String("stream_id", streamID))
	ctxzap.ToContext(ctx, l)

	// sendUpdate starts a new transaction and sends the current resource list.
	sendUpdate := func() {
		res, err := m.BuildDiscoveryResponse(streamID, resources)
		if err != nil {
			l.Error("problem building response", zap.Error(err))
			return
		}
		span := opentracing.StartSpan("xds_push", ext.SpanKindProducer)
		ext.PeerService.Set(span, node)
		span.SetTag("stream_id", streamID)
		span.SetTag("xds_type", m.Type)
		span.SetTag("xds_version", res.GetVersionInfo())
		t := &tx{start: time.Now(), span: span, version: res.GetVersionInfo(), nonce: res.GetNonce()}
		txs[res.GetNonce()] = t
		l.Info("pushing updated resources", zap.Object("tx", t), zap.Int("resource_count", len(res.Resources)))
		resCh <- res
		span.LogEvent("pushed resources")
	}

	// handleTx handles an acknowledgement
	handleTx := func(t *tx, req *envoy_api_v2.DiscoveryRequest) {
		t.span.LogEvent("got response")
		var ack bool
		origVersion, version := t.version, req.GetVersionInfo()
		if err := req.GetErrorDetail(); err != nil {
			ext.LogError(t.span, errors.New(err.GetMessage()))
			l.Error("envoy rejected configuration", zap.Any("error", err), zap.String("version.rejected", origVersion), zap.String("version.in_use", version), zap.Object("tx", t))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, origVersion, "NACK").Inc()
		} else {
			ack = true
			l.Info("envoy accepted configuration", zap.String("version.in_use", version), zap.String("version.sent", origVersion), zap.Object("tx", t))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, origVersion, "ACK").Inc()
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

	for {
		select {
		case <-server.Draining():
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
				zap.L().Warn("envoy changed resource subscriptions without opening a new stream", zap.Strings("new_resources", newResources))
				return status.Error(codes.FailedPrecondition, "resource subscriptions changed unexpectedly")
			}

			if t := req.GetTypeUrl(); t != m.Type {
				l.Warn("ignoring wrong-type discovery request", zap.String("manager_type", m.Type), zap.String("requested_type", t))
				continue
			}

			nonce := req.GetResponseNonce()
			if t, ok := txs[nonce]; ok {
				handleTx(t, req)
				break
			}
			if nonce == "" {
				l.Info("sending initial config")
			} else {
				l.Warn("envoy sent acknowledgement of unrecognized nonce; resending config", zap.String("nonce", nonce))
			}
			sendUpdate()
		case <-rCh:
			sendUpdate()
		}
	}
}

// XDSStream is the API shared among all envoy_api_v2.[type]DiscoveryService_Stream[type]Server
// streams.
type XDSStream interface {
	Context() context.Context
	Recv() (*envoy_api_v2.DiscoveryRequest, error)
	Send(*envoy_api_v2.DiscoveryResponse) error
}

// StreamGRPC adapts a gRPC stream of DiscoveryRequest -> DiscoveryResponse to the API required by
// the Stream function.
func (m *Manager) StreamGRPC(stream XDSStream) error {

	ctx := stream.Context()
	l := ctxzap.Extract(ctx)
	reqCh := make(chan *envoy_api_v2.DiscoveryRequest)
	resCh := make(chan *envoy_api_v2.DiscoveryResponse)
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
	jsonm := &jsonpb.Marshaler{EmitDefaults: verbose, OrigName: true}
	for _, r := range rs {
		j, err := jsonm.MarshalToString(r)
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
func (s *Manager) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, verbose := req.URL.Query()["verbose"]
	ya, err := s.ConfigAsYAML(verbose)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(ya)
}
