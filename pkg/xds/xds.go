// Package xds implements a CDS and EDS server.
package xds

import (
	"context"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server tracks the state of the Kubernetes cluster and sends updates to connected Envoy processes.
type Server struct{}

func (s *Server) StreamClusters(envoy_api_v2.ClusterDiscoveryService_StreamClustersServer) error {
	return status.Error(codes.Unimplemented, "unimplemented")
}
func (s *Server) DeltaClusters(envoy_api_v2.ClusterDiscoveryService_DeltaClustersServer) error {
	return status.Error(codes.Unimplemented, "unimplemented")
}
func (s *Server) FetchClusters(context.Context, *envoy_api_v2.DiscoveryRequest) (*envoy_api_v2.DiscoveryResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}
