package xds

import (
	"context"
	"sort"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/go-test/deep"
	"github.com/golang/protobuf/ptypes"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"sigs.k8s.io/yaml"
)

func TestManager(t *testing.T) {
	m := NewManager("test", "test-", &envoy_api_v2.Cluster{}, nil)
	if got, want := m.Type, "type.googleapis.com/envoy.api.v2.Cluster"; got != want {
		t.Errorf("computed type:\n  got: %v\n want: %v", got, want)
	}

	reqCh, resCh, errCh, ackCh := make(chan *envoy_api_v2.DiscoveryRequest), make(chan *envoy_api_v2.DiscoveryResponse), make(chan error), make(chan bool)

	l := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	m.Logger = l.Named("manager")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = ctxzap.ToContext(ctx, l.Named("stream"))
	m.OnAck = func(a Acknowledgment) {
		go func() { ackCh <- a.Ack }()
	}
	go func() { errCh <- m.Stream(ctx, reqCh, resCh) }()

	ack := func(version, nonce string) {
		t.Helper()
		reqCh <- &envoy_api_v2.DiscoveryRequest{
			VersionInfo:   version,
			Node:          &envoy_api_v2_core.Node{Id: "test"},
			TypeUrl:       m.Type,
			ResponseNonce: nonce,
		}
	}
	nack := func(version, nonce, error string) {
		t.Helper()
		reqCh <- &envoy_api_v2.DiscoveryRequest{
			VersionInfo:   version,
			Node:          &envoy_api_v2_core.Node{Id: "test"},
			TypeUrl:       m.Type,
			ResponseNonce: nonce,
			ErrorDetail: &status.Status{
				Code:    int32(codes.Unknown),
				Message: error,
			},
		}
	}
	assertClusters := func(want ...string) string {
		t.Helper()
		select {
		case res := <-resCh:
			rs := res.GetResources()
			var got []string
			for _, r := range rs {
				c := new(envoy_api_v2.Cluster)
				if err := ptypes.UnmarshalAny(r, c); err != nil {
					t.Fatalf("unmarshal cluster: %v", err)
				}
				got = append(got, c.GetName())
			}
			sort.Strings(got)
			sort.Strings(want)
			if diff := deep.Equal(got, want); diff != nil {
				t.Errorf("clusters:\n  got: %v\n want: %v\n diff: %#v", got, want, diff)
			}
			return res.GetNonce()
		case err := <-errCh:
			t.Fatalf("stream error while waiting for response (wanted: %v): %v", want, err)
		case <-ctx.Done():
			t.Fatalf("timeout while waiting for response (wanted: %v)", want)
		}
		return ""
	}
	assertAck := func(want bool) {
		t.Helper()
		select {
		case got := <-ackCh:
			if got != want {
				t.Errorf("ack:\n  got: %v\n want: %v", got, want)
			}
		case err := <-errCh:
			t.Fatalf("stream error while waiting for ack/nack (wanted: %v): %v", want, err)
		case <-ctx.Done():
			t.Fatalf("timeout while waiting for ack/nack (wanted: %v)", want)
		}
	}
	cs := func(names ...string) []Resource {
		var result []Resource
		for _, n := range names {
			result = append(result, &envoy_api_v2.Cluster{Name: n})
		}
		return result
	}

	// Initial request.
	ack("", "")
	n := assertClusters()
	ack("test-0", n)
	assertAck(true)

	// Push after adding a cluster.
	if err := m.Add(ctx, cs("foo")); err != nil {
		t.Fatalf("add: %v", err)
	}
	n = assertClusters("foo")
	ack("test-1", n)
	assertAck(true)

	// Push after deleting a cluster, which Envoy did not like!
	m.Delete(ctx, "foo")
	n = assertClusters()
	nack("test-1", n, "you deleted my favorite cluster!")
	assertAck(false)

	// Add a cluster again.
	if err := m.Add(ctx, cs("foo")); err != nil {
		t.Fatalf("add: %v", err)
	}
	n = assertClusters("foo")
	ack("test-3", n)
	assertAck(true)

	// Replace clusters.
	if err := m.Replace(ctx, cs("foo", "bar", "baz")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n = assertClusters("foo", "bar", "baz")
	ack("test-4", n)
	assertAck(true)

	// Replace clusters, send an incorrect ack.
	if err := m.Replace(ctx, cs("foo", "baz")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n = assertClusters("foo", "baz")
	ack("test-5", "nonce-completely-made-up")
	// Confuse xds again with a wrong version number.
	n = assertClusters("foo", "baz")
	ack("foo", n)
	assertAck(true)

	// Ask to resync.
	ack("", "")
	n = assertClusters("foo", "baz")
	ack("test-5", n)
	assertAck(true)

	// // Slip an invalid cluster into the pool.
	// NOTE(jrockway): Validate doesn't actually work on the resources when they're in the *any.Any form!
	// m.resources["bad"] = &envoy_api_v2.Cluster{Name: "bad", ConnectTimeout: ptypes.DurationProto(-1)}
	// m.Add(cs("bar"))
	// n = assertClusters("foo", "bad", "bar", "baz")

	cancel()
	select {
	case <-time.After(time.Second):
		t.Fatal("stream did not exit")
	case <-errCh:
	}
}

func TestNamedSubscriptions(t *testing.T) {
	m := NewManager("named-subscriptions", "named-subscriptions-", &envoy_api_v2.ClusterLoadAssignment{}, nil)
	reqCh, resCh, errCh := make(chan *envoy_api_v2.DiscoveryRequest), make(chan *envoy_api_v2.DiscoveryResponse), make(chan error)

	l := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	m.Logger = l.Named("manager")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = ctxzap.ToContext(ctx, l.Named("stream"))
	go func() { errCh <- m.Stream(ctx, reqCh, resCh) }()

	select {
	case reqCh <- &envoy_api_v2.DiscoveryRequest{
		VersionInfo:   "",
		Node:          &envoy_api_v2_core.Node{Id: "test2"},
		TypeUrl:       m.Type,
		ResourceNames: []string{"foo"},
	}:
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	var res *envoy_api_v2.DiscoveryResponse
	select {
	case res = <-resCh:
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	if got, want := len(res.GetResources()), 0; got != want {
		t.Fatal("unexpected resource recieved")
	}

	// This won't block, because there are no receivers to notify.
	m.Add(ctx, []Resource{&envoy_api_v2.ClusterLoadAssignment{ClusterName: "bar"}})
	select {
	case res = <-resCh:
		t.Fatalf("unexpected recv %v", res)
	case <-ctx.Done():
		t.Fatal("unexpected timeout")
	default:
	}

	// This one will block.
	go m.Add(ctx, []Resource{&envoy_api_v2.ClusterLoadAssignment{ClusterName: "foo"}})
	select {
	case res = <-resCh:
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	if got, want := len(res.GetResources()), 1; got != want {
		t.Fatalf("unexpected resource recieved:\n  got: %v\n want: %v", got, want)
	}

	cancel()
	select {
	case <-time.After(time.Second):
		t.Fatal("stream did not exit")
	case <-errCh:
	}
}

func TestConfigAsYAML(t *testing.T) {
	s := NewManager("test", "", &envoy_api_v2.Cluster{}, nil)
	err := s.Add(context.Background(), []Resource{&envoy_api_v2.Cluster{Name: "foo"}})
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := s.ConfigAsYAML(false)
	if err != nil {
		t.Fatal(err)
	}
	js, err := yaml.YAMLToJSON(bytes)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"resources":[{"name":"foo"}]}`
	if got := string(js); got != want {
		t.Errorf("yaml:\n  got: %v\n want: %v", got, want)
	}
}
