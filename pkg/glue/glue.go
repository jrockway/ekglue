// Package glue glues Kubernetes to Envoy
package glue

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/xds"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

type ClusterConfig struct {
	//NameTemplate string                `json:"name_template"`
	BaseConfig *envoy_api_v2.Cluster `json:"base"`
}

type Config struct {
	ClusterConfig *ClusterConfig `json:"cluster_config"`
}

func DefaultConfig() *Config {
	return &Config{
		ClusterConfig: &ClusterConfig{
			BaseConfig: &envoy_api_v2.Cluster{
				ConnectTimeout: ptypes.DurationProto(time.Second),
			},
		},
	}
}

// Base returns a deep copy of the base cluster configuration.
func (c *ClusterConfig) GetBaseConfig() *envoy_api_v2.Cluster {
	raw := proto.Clone(c.BaseConfig)
	cluster, ok := raw.(*envoy_api_v2.Cluster)
	if !ok {
		zap.L().Error("internal error: couldn't clone ClusterConfig.BaseConfig")
		return &envoy_api_v2.Cluster{}
	}
	return cluster
}

// this is not a logical factoring of this operation, it's strictly for convenience, laziness, and
// other bad reasons.
func singleTargetLoadAssignment(cluster, hostname string, port int32) *envoy_api_v2.ClusterLoadAssignment {
	return &envoy_api_v2.ClusterLoadAssignment{
		ClusterName: cluster,
		Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{{
			LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{{
				HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
					Endpoint: &envoy_api_v2_endpoint.Endpoint{
						Address: &envoy_api_v2_core.Address{
							Address: &envoy_api_v2_core.Address_SocketAddress{
								SocketAddress: &envoy_api_v2_core.SocketAddress{
									Address: hostname,
									PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
										PortValue: uint32(port),
									},
								},
							},
						},
					},
				},
			}},
		}},
	}
}

// ClustersFromService translates a Kubernetes service into a set of Envoy clusters according to the
// config (1 cluster per service port).
func (c *ClusterConfig) ClustersFromService(svc *v1.Service) []*envoy_api_v2.Cluster {
	var result []*envoy_api_v2.Cluster
	if svc == nil {
		return nil
	}
	for _, port := range svc.Spec.Ports {
		cl := c.GetBaseConfig()
		n := port.Name
		if n == "" {
			n = strconv.Itoa(int(port.Port))
		}
		cl.Name = fmt.Sprintf("%s:%s:%s", svc.GetNamespace(), svc.GetName(), n)
		cl.ClusterDiscoveryType = &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS}
		cl.LoadAssignment = singleTargetLoadAssignment(cl.Name, fmt.Sprintf("%s.%s.svc.cluster.local.", svc.GetName(), svc.GetNamespace()), port.Port)
		result = append(result, cl)
	}
	return result
}

// ClusterStore is a cache.Store that receives updates about the status of Kubernetes services,
// translates the services to Envoy cluster objects with the provided config, and reports those
// clusters to the xDS server.
type ClusterStore struct {
	cfg *ClusterConfig
	s   *xds.Server
}

// Store returns a cache.Store that allows a Kubernetes reflector to sync service changes to a CDS
// server.
func (c *ClusterConfig) Store(s *xds.Server) *ClusterStore {
	return &ClusterStore{
		cfg: c,
		s:   s,
	}
}

func (cs *ClusterStore) Add(obj interface{}) error {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("add service: got non-service object %#v", obj)
	}
	if err := cs.s.AddClusters(cs.cfg.ClustersFromService(svc)); err != nil {
		return fmt.Errorf("add service: clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Update(obj interface{}) error {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("update service: got non-service object %#v", obj)
	}
	if err := cs.s.AddClusters(cs.cfg.ClustersFromService(svc)); err != nil {
		return fmt.Errorf("update service: add clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Delete(obj interface{}) error {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("delete service: got non-service object %#v", obj)
	}
	clusters := cs.cfg.ClustersFromService(svc)
	for _, c := range clusters {
		cs.s.DeleteCluster(c.GetName())
	}
	return nil
}
func (cs *ClusterStore) List() []interface{} {
	clusters := cs.s.ListClusters()
	var result []interface{}
	for _, c := range clusters {
		result = append(result, c)
	}
	return result
}

func (cs *ClusterStore) ListKeys() []string {
	clusters := cs.s.ListClusters()
	var result []string
	for _, c := range clusters {
		result = append(result, c.GetName())
	}
	return result
}
func (cs *ClusterStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}
func (cs *ClusterStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}
func (cs *ClusterStore) Replace(objs []interface{}, _ string) error {
	var clusters []*envoy_api_v2.Cluster
	for _, obj := range objs {
		svc, ok := obj.(*v1.Service)
		if !ok {
			return fmt.Errorf("replace services: got non-service object %#v", obj)
		}
		clusters = append(clusters, cs.cfg.ClustersFromService(svc)...)
	}
	if err := cs.s.ReplaceClusters(clusters); err != nil {
		return fmt.Errorf("replace services: replace clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Resync() error {
	return errors.New("clusterwatcher.Resync unimplemented")
}
