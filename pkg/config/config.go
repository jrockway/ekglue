// Package config configures the xDS servers.
package config

import (
	"fmt"
	"strconv"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

type ClusterConfig struct {
	NameTemplate string                `json:"name_template"`
	BaseConfig   *envoy_api_v2.Cluster `json:"base"`
}

type Config struct {
	ClusterConfig *ClusterConfig `json:"cluster_config"`
}

func NewConfig() *Config {
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

// ClustersFromService translates a Kubernetes service into a set of Envoy clusters according to the
// config (1 cluster per service port).
func (c *ClusterConfig) ClustersFromService(svc *v1.Service) []*envoy_api_v2.Cluster {
	var result []*envoy_api_v2.Cluster
	for _, port := range svc.Spec.Ports {
		cl := c.GetBaseConfig()
		n := port.Name
		if n == "" {
			n = strconv.Itoa(int(port.Port))
		}
		cl.Name = fmt.Sprintf("%s:%s:%s", svc.GetNamespace(), svc.GetName(), n)
		cl.ClusterDiscoveryType = &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS}
		cl.LoadAssignment = &envoy_api_v2.ClusterLoadAssignment{
			ClusterName: cl.Name,
			Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{{
				LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{{
					HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
						Endpoint: &envoy_api_v2_endpoint.Endpoint{
							Address: &envoy_api_v2_core.Address{
								Address: &envoy_api_v2_core.Address_SocketAddress{
									SocketAddress: &envoy_api_v2_core.SocketAddress{
										Address: fmt.Sprintf("%s.%s.svc.cluster.local.", svc.GetName(), svc.GetNamespace()),
										PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
											PortValue: uint32(port.Port),
										},
									},
								},
							},
						},
					},
				}},
			}},
		}
		result = append(result, cl)
	}
	return result
}
