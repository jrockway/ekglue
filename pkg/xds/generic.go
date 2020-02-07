package xds

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/jrockway/opinionated-server/server"
	"github.com/opentracing/opentracing-go"
	"go.uber.org/zap"
)

// Resource is an xDS resource, like envoy_api_v2.Cluster, etc.
type Resource interface {
	proto.Message
	GetName() string
	Validate() error
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
func NewManager(name, versionPrefix string, resource interface{}) (*Manager, error) {
	res, ok := resource.(Resource)
	if !ok {
		return nil, fmt.Errorf("resource type %t cannot be managed", resource)
	}
	m := &Manager{
		Name:          name,
		VersionPrefix: versionPrefix,
		Type:          "type.googleapis.com/" + proto.MessageName(res),
		Logger:        zap.L().Named(name),
		resources:     make(map[string]Resource),
		sessions:      make(map[session]struct{}),
	}
	return m, nil
}

// version returns the version number of the current config.  You must hold the Manager's lock.
func (m *Manager) versionString() string {
	return fmt.Sprintf("%s%d", m.VersionPrefix, m.version)
}

// snapshot returns the current list of managed resources.  You must hold the Manager's lock.
func (m *Manager) snapshot() ([]*any.Any, string, error) {
	result := make([]*any.Any, 0, len(m.resources))
	for _, r := range m.resources {
		any, err := ptypes.MarshalAny(r)
		if err != nil {
			return nil, "", fmt.Errorf("marshal resource %s to any: %w", r, err)
		}
		result = append(result, any)
	}
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
		n := r.GetName()
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
			return fmt.Errorf("%q: %w", r.GetName(), err)
		}
	}
	m.Lock()
	defer m.Unlock()
	old := m.resources
	m.resources = make(map[string]Resource)
	for _, r := range rs {
		n := r.GetName()
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
}

// ListKeys returns the sorted names of managed resources.
func (m *Manager) ListKeys() []string {
	m.Lock()
	defer m.Unlock()
	result := make([]string, 0, len(m.resources))
	for _, r := range m.resources {
		result = append(result, r.GetName())
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
		return result[i].GetName() < result[j].GetName()
	})
	return result
}

type tx struct {
	span    opentracing.Span
	nonce   string
	version string
}

func (m *Manager) BuildDiscoveryResponse() (*envoy_api_v2.DiscoveryResponse, error) {
	m.Lock()
	defer m.Unlock()
	resources, version, err := m.snapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshot resources: %w", err)
	}
	res := &envoy_api_v2.DiscoveryResponse{
		VersionInfo: version,
		TypeUrl:     m.Type,
		Resources:   resources,
		Nonce:       fmt.Sprintf("nonce-%s", version),
	}
	if err := res.Validate(); err != nil {
		return nil, fmt.Errorf("validate generated discovery response: %w", err)
	}
	return res, nil
}

// Stream manages a client connection.  Requests from the client are read from reqCh, responses are
// written to resCh, and the function returns when no further progress can be made.
func (m *Manager) Stream(ctx context.Context, reqCh chan *envoy_api_v2.DiscoveryRequest, resCh chan *envoy_api_v2.DiscoveryResponse) error {
	l := ctxzap.Extract(ctx)

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

	// sendUpdate starts a new transaction and sends the current resource list.
	sendUpdate := func() {
		res, err := m.BuildDiscoveryResponse()
		if err != nil {
			l.Error("problem building response", zap.Error(err))
		}
		span := opentracing.StartSpan("xds_push")
		txs[res.GetNonce()] = &tx{span: span, version: res.GetVersionInfo(), nonce: res.GetNonce()}
		resCh <- res
	}

	// handleTx handles an acknowledgement
	handleTx := func(t *tx, req *envoy_api_v2.DiscoveryRequest) {
		var ack bool
		origVersion, version := t.version, req.GetVersionInfo()
		if err := req.GetErrorDetail(); err != nil {
			l.Error("envoy rejected configuration", zap.Any("error", err), zap.String("version.rejected", origVersion), zap.String("version.in_use", version))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, origVersion, "NACK").Inc()
		} else {
			ack = true
			l.Info("envoy accepted configuration", zap.String("version.in_use", version), zap.String("version.sent", origVersion))
			xdsConfigAcceptanceStatus.WithLabelValues(m.Name, m.Type, origVersion, "ACK").Inc()
		}

		if f := m.OnAck; f != nil {
			f(Acknowledgment{
				Ack:     ack,
				Node:    node,
				Version: origVersion,
			})
		}
		delete(txs, req.GetResponseNonce())
	}

	for {
		select {
		case <-server.Draining():
			return errors.New("server draining")
		case <-ctx.Done():
			return ctx.Err()
		case req, ok := <-reqCh:
			if !ok {
				return errors.New("request channel closed")
			}
			if node == "" {
				node = req.GetNode().GetId()
				l = l.With(zap.String("envoy.node.id", node))
				ctxzap.ToContext(ctx, l)
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
			l.Info("first request or invalid nonce state; sending resources")
			sendUpdate()
		case <-rCh:
			l.Info("resources updated; sending resources")
			sendUpdate()
		}
	}
}
