// Package cds implements a CDS/EDS server.
package cds

import (
	"context"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Number of Envoy instances with an open CDS stream.
	cdsClientsStreaming = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ekglue_cds_active_stream_count",
		Help: "The number of clients connected and streaming cluster updates.",
	})
	edsClientsStreaming = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ekglue_eds_active_stream_count",
		Help: "The number of clients connected and streaming endpoint updates.",
	})
)

// Server is a CDS and EDS server.
type Server struct {
	// We do not implement the GRPC_DELTA or REST protocols.  We include this to pick up stubs
	// for those methods, and any future protocols that are added.
	clusterservice.UnimplementedClusterDiscoveryServiceServer
	endpointservice.UnimplementedEndpointDiscoveryServiceServer

	Clusters, Endpoints *xds.Manager
}

// NewServer returns a new server that is ready to serve.
func NewServer(versionPrefix string, drainCh chan struct{}) *Server {
	return &Server{
		Clusters:  xds.NewManager("clusters", versionPrefix, &envoy_config_cluster_v3.Cluster{}, drainCh),
		Endpoints: xds.NewManager("endpoints", versionPrefix, &envoy_config_endpoint_v3.ClusterLoadAssignment{}, drainCh),
	}
}

func resourcesToClusters(rs []xds.Resource) []*envoy_config_cluster_v3.Cluster {
	result := make([]*envoy_config_cluster_v3.Cluster, len(rs))
	for i, r := range rs {
		result[i] = r.(*envoy_config_cluster_v3.Cluster)
	}
	return result
}

// who needs generics when we have for loops!?
func clustersToResources(cs []*envoy_config_cluster_v3.Cluster) []xds.Resource {
	result := make([]xds.Resource, len(cs))
	for i, c := range cs {
		result[i] = c
	}
	return result
}

func resourcesToLoadAssignments(rs []xds.Resource) []*envoy_config_endpoint_v3.ClusterLoadAssignment {
	result := make([]*envoy_config_endpoint_v3.ClusterLoadAssignment, len(rs))
	for i, r := range rs {
		result[i] = r.(*envoy_config_endpoint_v3.ClusterLoadAssignment)
	}
	return result
}

func loadAssignmentsToResources(cs []*envoy_config_endpoint_v3.ClusterLoadAssignment) []xds.Resource {
	result := make([]xds.Resource, len(cs))
	for i, c := range cs {
		result[i] = c
	}
	return result
}

// ListClusters returns the clusters that we are managing.  Meant to mirror kubernetes's cache.Store
// API.
func (s *Server) ListClusters() []*envoy_config_cluster_v3.Cluster {
	return resourcesToClusters(s.Clusters.List())
}

// AddClusters adds or updates clusters, and notifies all connected clients of the change.
func (s *Server) AddClusters(ctx context.Context, cs []*envoy_config_cluster_v3.Cluster) error {
	return s.Clusters.Add(ctx, clustersToResources(cs))
}

// DeleteCluster deletes a cluster by name, and notifies all connected clients of the change.
func (s *Server) DeleteCluster(ctx context.Context, name string) {
	s.Clusters.Delete(ctx, name)
}

// ReplaceClusters replaces all tracked clusters with a new list of clusters.
func (s *Server) ReplaceClusters(ctx context.Context, cs []*envoy_config_cluster_v3.Cluster) error {
	return s.Clusters.Replace(ctx, clustersToResources(cs))
}

// ListEndpoints returns the load assignments / endpoints that we are managing.
func (s *Server) ListEndpoints() []*envoy_config_endpoint_v3.ClusterLoadAssignment {
	return resourcesToLoadAssignments(s.Endpoints.List())
}

// AddEndpoints adds or updates endpoints, and notifies all connected clients of the change.
func (s *Server) AddEndpoints(ctx context.Context, es []*envoy_config_endpoint_v3.ClusterLoadAssignment) error {
	return s.Endpoints.Add(ctx, loadAssignmentsToResources(es))
}

// DeleteEndpoints deletes a load assignment by name, and notifies all connected clients of the
// change.  A load assignment may contain many endpoints, this deletes them all.
func (s *Server) DeleteEndpoints(ctx context.Context, name string) {
	s.Endpoints.Delete(ctx, name)
}

// ReplaceEndpoints replaces all load assignments with a new set of load assignments.
func (s *Server) ReplaceEndpoints(ctx context.Context, es []*envoy_config_endpoint_v3.ClusterLoadAssignment) error {
	return s.Endpoints.Replace(ctx, loadAssignmentsToResources(es))
}

// StreamClusters implements CDS.
func (s *Server) StreamClusters(stream clusterservice.ClusterDiscoveryService_StreamClustersServer) error {
	cdsClientsStreaming.Inc()
	defer cdsClientsStreaming.Dec()
	return s.Clusters.StreamGRPC(stream)
}

// StreamEndpoints implements EDS.
func (s *Server) StreamEndpoints(stream endpointservice.EndpointDiscoveryService_StreamEndpointsServer) error {
	edsClientsStreaming.Inc()
	defer edsClientsStreaming.Dec()
	return s.Endpoints.StreamGRPC(stream)
}
