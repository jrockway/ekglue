// Package glue glues Kubernetes to Envoy
package glue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/cds"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var (
	// For extreme debugging, you can overwrite this.
	Logger = zap.NewNop()
)

type Matcher struct {
	// ClusterName matches the mangled name of a cluster (not the original service name).
	ClusterName string `json:"cluster_name"`
}

type ClusterOverride struct {
	// Match specifies a cluster to match; multiple items are OR'd.
	Match []*Matcher `json:"match"`
	// Configuration to override if a matcher matches.
	Override *envoy_api_v2.Cluster `json:"override"`
}

func (o *ClusterOverride) UnmarshalJSON(b []byte) error {
	tmp := struct {
		Match    []*Matcher      `json:"match"`
		Override json.RawMessage `json:"override"`
	}{}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return fmt.Errorf("ClusterOverride: unmarshal into temporary structure: %w", err)
	}
	o.Match = tmp.Match
	base := &envoy_api_v2.Cluster{}
	if err := jsonpb.UnmarshalString(string(tmp.Override), base); err != nil {
		return fmt.Errorf("ClusterOverride: unmarshal Override: %w", err)
	}
	o.Override = base
	return nil
}

type ClusterConfig struct {
	// The base configuration that should be used for all clusters.
	BaseConfig *envoy_api_v2.Cluster `json:"base"`
	// Any rule-based overrides.
	Overrides []*ClusterOverride `json:"overrides"`
}

func (c *ClusterConfig) UnmarshalJSON(b []byte) error {
	tmp := struct {
		BaseConfig json.RawMessage    `json:"base"`
		Overrides  []*ClusterOverride `json:"overrides"`
	}{}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return fmt.Errorf("ClusterConfig: unmarshal into temporary structure: %w", err)
	}
	c.Overrides = tmp.Overrides

	base := &envoy_api_v2.Cluster{}
	if err := jsonpb.UnmarshalString(string(tmp.BaseConfig), base); err != nil {
		return fmt.Errorf("ClusterConfig: unmarshal BaseConfig %s: %w", tmp.BaseConfig, err)
	}
	base.Name = "XXX" // required for validation, but we will always add it ourselves later
	if err := base.Validate(); err != nil {
		return fmt.Errorf("ClusterConfig: validate config base: %w", err)
	}
	base.Name = ""
	c.BaseConfig = base
	return nil
}

type Config struct {
	ApiVersion    string         `json:"apiVersion"`
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

func LoadConfig(filename string) (*Config, error) {
	raw, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("converting YAML to JSON: %w", err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(js, cfg); err != nil {
		return nil, err
	}
	if v := cfg.ApiVersion; v != "v1alpha" {
		return nil, fmt.Errorf("unknown config version %q; expected v1alpha", v)
	}
	return cfg, nil
}

// Base returns a deep copy of the base cluster configuration.
func (c *ClusterConfig) GetBaseConfig() *envoy_api_v2.Cluster {
	raw := proto.Clone(c.BaseConfig)
	cluster, ok := raw.(*envoy_api_v2.Cluster)
	if !ok {
		zap.L().Fatal("internal error: couldn't clone ClusterConfig.BaseConfig")
	}
	return cluster
}

// GetOverride returns the override configuration for the provided service.
func (c *ClusterConfig) GetOverride(cluster *envoy_api_v2.Cluster, svc *v1.Service, port v1.ServicePort) *envoy_api_v2.Cluster {
	base := &envoy_api_v2.Cluster{}
	for _, o := range c.Overrides {
		if o.Override == nil {
			continue
		}
		for _, m := range o.Match {
			if m.ClusterName == cluster.GetName() {
				proto.Merge(base, o.Override)
				break
			}
		}
	}
	return base
}

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

func (c *ClusterConfig) isEDS(cl *envoy_api_v2.Cluster) bool {
	dtype := cl.GetClusterDiscoveryType()
	if dtype == nil {
		return false
	}
	return cl.GetType() == envoy_api_v2.Cluster_EDS
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
		proto.Merge(cl, c.GetOverride(cl, svc, port))
		if !c.isEDS(cl) {
			if cl.ClusterDiscoveryType == nil {
				cl.ClusterDiscoveryType = &envoy_api_v2.Cluster_Type{
					Type: envoy_api_v2.Cluster_STRICT_DNS,
				}
			}
			cl.LoadAssignment = singleTargetLoadAssignment(cl.Name, fmt.Sprintf("%s.%s.svc.cluster.local.", svc.GetName(), svc.GetNamespace()), port.Port)
		}
		result = append(result, cl)
	}
	return result
}

// ClusterStore is a cache.Store that receives updates about the status of Kubernetes services,
// translates the services to Envoy cluster objects with the provided config, and reports those
// clusters to the xDS server.
type ClusterStore struct {
	cfg *ClusterConfig
	s   *cds.Server
}

// Store returns a cache.Store that allows a Kubernetes reflector to sync service changes to a CDS
// server.
func (c *ClusterConfig) Store(s *cds.Server) *ClusterStore {
	return &ClusterStore{
		cfg: c,
		s:   s,
	}
}

func (cs *ClusterStore) Add(obj interface{}) error {
	Logger.Debug("Add")
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
	Logger.Debug("Update")
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
	Logger.Debug("Delete")
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
	Logger.Debug("List")
	clusters := cs.s.ListClusters()
	var result []interface{}
	for _, c := range clusters {
		result = append(result, c)
	}
	return result
}

func (cs *ClusterStore) ListKeys() []string {
	Logger.Debug("ListKeys")
	clusters := cs.s.ListClusters()
	var result []string
	for _, c := range clusters {
		result = append(result, c.GetName())
	}
	return result
}
func (cs *ClusterStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	Logger.Debug("Get")
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}
func (cs *ClusterStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	Logger.Debug("GetByKey")
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}
func (cs *ClusterStore) Replace(objs []interface{}, _ string) error {
	Logger.Debug("Replace")
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
	Logger.Debug("Resync")
	return errors.New("clusterwatcher.Resync unimplemented")
}
