package cds

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery_v3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/jrockway/ekglue/pkg/cds/internal/fakexds"
	"github.com/jrockway/ekglue/pkg/xds"
	"google.golang.org/genproto/googleapis/rpc/status"
)

func requestClusters(version, nonce string, err *status.Status) *discovery_v3.DiscoveryRequest {
	return &discovery_v3.DiscoveryRequest{
		VersionInfo:   version,
		ResponseNonce: nonce,
		ErrorDetail:   err,
		TypeUrl:       "type.googleapis.com/envoy.config.cluster.v3.Cluster",
		Node: &envoy_config_core_v3.Node{
			Id: "unit-tests",
		},
	}
}

func clustersFromResponse(res *discovery_v3.DiscoveryResponse) ([]string, error) {
	var result []string
	for _, a := range res.GetResources() {
		cluster := new(envoy_config_cluster_v3.Cluster)
		if err := a.UnmarshalTo(cluster); err != nil {
			return nil, err
		}
		result = append(result, cluster.GetName())
	}
	sort.Strings(result)
	return result, nil
}

func TestCDSFlow(t *testing.T) {
	s := NewServer("test", nil)
	ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()

	ackCh := make(chan struct{})
	s.Clusters.OnAck = func(a xds.Acknowledgment) {
		ackCh <- struct{}{}
	}

	if err := s.AddClusters(ctx, []*envoy_config_cluster_v3.Cluster{{Name: "a"}}); err != nil {
		t.Fatalf("adding cluster 'a': %v", err)
	}

	doneCh := make(chan error)
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	ctx = ctxzap.ToContext(ctx, logger)
	stream := fakexds.NewStream(ctx)
	go func() {
		err := s.StreamClusters(stream)
		close(ackCh)
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
	case <-ackCh:
	case <-ctx.Done():
		t.Fatal("context done while waiting for 1st ack")
	}

	if err := s.AddClusters(ctx, []*envoy_config_cluster_v3.Cluster{{Name: "bad"}}); err != nil {
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
	case <-ackCh:
	case <-ctx.Done():
		t.Fatal("context done while waiting for 2nd ack")
	}
	done()
	finalErr := <-doneCh
	if err != nil && !errors.Is(finalErr, context.Canceled) {
		t.Fatalf("server stopped for an unexpected reason: %v", err)
	}
}
