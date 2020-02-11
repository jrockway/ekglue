// Package cds implements a CDS/EDS server.
package cds

import (
	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Number of Envoy instances with an open CDS stream.
	cdsClientsStreaming = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cds_active_stream_count",
		Help: "The number of clients connected and streaming cluster updates.",
	})
	edsClientsStreaming = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "eds_active_stream_count",
		Help: "The number of clients connected and streaming endpoint updates.",
	})
)

// Server is a CDS and EDS server.
type Server struct {
	// We do not implement the GRPC_DELTA or REST protocols.  We include this to pick up stubs
	// for those methods, and any future protocols that are added.
	envoy_api_v2.UnimplementedClusterDiscoveryServiceServer
	envoy_api_v2.UnimplementedEndpointDiscoveryServiceServer

	Clusters, Endpoints *xds.Manager
}

// NewServer returns a new server that is ready to serve.
func NewServer(versionPrefix string) *Server {
	return &Server{
		Clusters:  xds.NewManager("clusters", versionPrefix, &envoy_api_v2.Cluster{}),
		Endpoints: xds.NewManager("endpoints", versionPrefix, &envoy_api_v2.ClusterLoadAssignment{}),
	}
}

func resourcesToClusters(rs []xds.Resource) []*envoy_api_v2.Cluster {
	result := make([]*envoy_api_v2.Cluster, len(rs))
	for i, r := range rs {
		result[i] = r.(*envoy_api_v2.Cluster)
	}
	return result
}

// who needs generics when we have for loops!?
func clustersToResources(cs []*envoy_api_v2.Cluster) []xds.Resource {
	result := make([]xds.Resource, len(cs))
	for i, c := range cs {
		result[i] = c
	}
	return result
}

// ListClusters returns the clusters that we are managing.  Meant to mirror kubernetes's cache.Store
// API.
func (s *Server) ListClusters() []*envoy_api_v2.Cluster {
	return resourcesToClusters(s.Clusters.List())
}

// AddClusters adds or updates clusters, and notifies all connected clients of the change.
func (s *Server) AddClusters(cs []*envoy_api_v2.Cluster) error {
	return s.Clusters.Add(clustersToResources(cs))
}

// DeleteCluster deletes a cluster by name, and notifies all connected clients of the change.
func (s *Server) DeleteCluster(name string) {
	s.Clusters.Delete(name)
}

// ReplaceClusters replaces all tracked clusters with a new list of clusters.
func (s *Server) ReplaceClusters(cs []*envoy_api_v2.Cluster) error {
	return s.Clusters.Replace(clustersToResources(cs))
}

// feedXDS pipes discovery requests/responses from the gRPC API to

// StreamClusters implements CDS.
func (s *Server) StreamClusters(stream envoy_api_v2.ClusterDiscoveryService_StreamClustersServer) error {
	cdsClientsStreaming.Inc()
	defer cdsClientsStreaming.Dec()
	return s.Clusters.StreamGRPC(stream)
}

// StreamEndpoints implements EDS.
func (s *Server) StreamEndpoints(stream envoy_api_v2.EndpointDiscoveryService_StreamEndpointsServer) error {
	edsClientsStreaming.Inc()
	defer edsClientsStreaming.Dec()
	return s.Endpoints.StreamGRPC(stream)
}
