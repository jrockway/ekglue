package xds

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/xds/internal/fakexds"
	"google.golang.org/genproto/googleapis/rpc/status"
)

func requestClusters(version string, nonce string, err *status.Status) *envoy_api_v2.DiscoveryRequest {
	return &envoy_api_v2.DiscoveryRequest{
		VersionInfo:   version,
		ResponseNonce: nonce,
		ErrorDetail:   err,
		TypeUrl:       "type.googleapis.com/envoy.api.v2.Cluster",
		Node: &core.Node{
			Id: "unit-tests",
		},
	}
}

func clustersFromResponse(res *envoy_api_v2.DiscoveryResponse) ([]string, error) {
	var result []string
	for _, a := range res.GetResources() {
		cluster := new(envoy_api_v2.Cluster)
		if err := ptypes.UnmarshalAny(a, cluster); err != nil {
			return nil, err
		}
		result = append(result, cluster.GetName())
	}
	sort.Strings(result)
	return result, nil
}

func TestCDSFlow(t *testing.T) {
	s := NewServer()
	s.ackCh = make(chan struct{})
	if err := s.AddClusters([]*envoy_api_v2.Cluster{{Name: "a"}}); err != nil {
		t.Fatalf("adding cluster 'a': %v", err)
	}

	doneCh := make(chan error)
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	ctx = ctxzap.ToContext(ctx, logger)
	stream := fakexds.NewStream(ctx)
	go func() {
		err := s.StreamClusters(stream)
		close(s.ackCh)
		doneCh <- err
	}()

	res, err := stream.RequestAndWait(requestClusters("", "", nil))
	if err != nil {
		t.Fatalf("initial cluster fetch: %v", err)
	}
	got, err := clustersFromResponse(res)
	if err != nil {
		t.Fatalf("read clusters from response: %v", err)
	}
	if got, want := got, []string{"a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("initial cluster fetch: cluster names:\n  got: %v\n want: %v", got, want)
	}
	if err := stream.Request(requestClusters(res.GetVersionInfo(), res.GetNonce(), nil)); err != nil {
		t.Fatalf("sending ACK failed: %v", err)
	}
	goodVersion := res.GetVersionInfo()
	select {
	case <-s.ackCh:
	case <-ctx.Done():
		t.Fatal("context done while waiting for 1st ack")
	}

	if err := s.AddClusters([]*envoy_api_v2.Cluster{{Name: "bad"}}); err != nil {
		t.Fatalf("adding cluster 'bad': %v", err)
	}
	res, err = stream.Await()
	if err != nil {
		t.Fatalf("await cluster push: %v", err)
	}
	got, err = clustersFromResponse(res)
	if err != nil {
		t.Fatalf("read cluster push: %v", err)
	}
	if got, want := got, []string{"a", "bad"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster push: cluster names:\n  got: %v\n want: %v", got, want)
	}
	if err := stream.Request(requestClusters(goodVersion, res.GetNonce(), &status.Status{})); err != nil {
		t.Fatalf("sending NACK failed: %v", err)
	}
	select {
	case <-s.ackCh:
	case <-ctx.Done():
		t.Fatal("context done while waiting for 2nd ack")
	}
	done()
	finalErr := <-doneCh
	if err != nil && !errors.Is(finalErr, context.Canceled) {
		t.Fatalf("server stopped for an unexpected reason: %v", err)
	}
}