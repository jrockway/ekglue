// Package xds implements a CDS and EDS server.
package xds

import (
	"net/http"
	"sort"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_config_bootstrap_v2 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v2"
	"github.com/golang/protobuf/jsonpb"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"sigs.k8s.io/yaml"
)

// cdsSession represents an RPC stream subscribed to cluster updates.
type cdsSession chan struct{}

// Server is a CDS server.
type Server struct {
	// We do not implement the GRPC_DELTA or REST protocols.  We include this to pick up stubs
	// for those methods, and any future protocols that are added.
	envoy_api_v2.UnimplementedClusterDiscoveryServiceServer

	cm *Manager
}

// NewServer returns a new server that is ready to serve.
func NewServer(versionPrefix string) *Server {
	m, err := NewManager("clusters", versionPrefix, &envoy_api_v2.Cluster{})
	if err != nil {
		panic(err)
	}
	return &Server{
		cm: m,
	}
}

func resourcesToClusters(rs []Resource) []*envoy_api_v2.Cluster {
	result := make([]*envoy_api_v2.Cluster, len(rs))
	for i, r := range rs {
		result[i] = r.(*envoy_api_v2.Cluster)
	}
	return result
}

// who needs generics when we have for loops!?
func clustersToResources(cs []*envoy_api_v2.Cluster) []Resource {
	result := make([]Resource, len(cs))
	for i, c := range cs {
		result[i] = c
	}
	return result
}

// ListClusters returns the clusters that we are managing.  Meant to mirror kubernetes's cache.Store
// API.
func (s *Server) ListClusters() []*envoy_api_v2.Cluster {
	return resourcesToClusters(s.cm.List())
}

// AddClusters adds or updates clusters, and notifies all connected clients of the change.
func (s *Server) AddClusters(cs []*envoy_api_v2.Cluster) error {
	return s.cm.Add(clustersToResources(cs))
}

// DeleteCluster deletes a cluster by name, and notifies all connected clients of the change.
func (s *Server) DeleteCluster(name string) {
	s.cm.Delete(name)
}

// ReplaceClusters replaces all tracked clusters with a new list of clusters.
func (s *Server) ReplaceClusters(cs []*envoy_api_v2.Cluster) error {
	return s.cm.Replace(clustersToResources(cs))
}

// StreamClusters implements CDS.
func (s *Server) StreamClusters(stream envoy_api_v2.ClusterDiscoveryService_StreamClustersServer) error {
	cdsClientsStreaming.Inc()
	defer cdsClientsStreaming.Dec()

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

	go func() { errCh <- s.cm.Stream(ctx, reqCh, resCh) }()
	err := <-errCh
	close(resCh)
	close(errCh)
	return err
}

// ConfigAsYAML dumps the currently-tracked clusters as YAML.
func (s *Server) ConfigAsYAML(verbose bool) ([]byte, error) {
	clusters := s.ListClusters()
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].GetName() < clusters[j].GetName()
	})
	bs := &envoy_config_bootstrap_v2.Bootstrap{
		StaticResources: &envoy_config_bootstrap_v2.Bootstrap_StaticResources{
			Clusters: clusters,
		},
	}
	js, err := (&jsonpb.Marshaler{EmitDefaults: verbose, OrigName: true}).MarshalToString(bs)
	if err != nil {
		return nil, err
	}
	ya, err := yaml.JSONToYAML([]byte(js))
	if err != nil {
		return nil, err
	}
	return ya, nil

}

// ServeHTTP dumps the currently-tracked clusters as YAML.
//
// It will normally omit defaults, but with "?verbose" in the query params, it will print those too.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, verbose := req.URL.Query()["verbose"]
	ya, err := s.ConfigAsYAML(verbose)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(ya)
}
