// Package glue glues Kubernetes to Envoy
package glue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"sort"
	"strconv"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

var (
	// For extreme debugging, you can overwrite this.
	Logger = zap.NewNop()

	k8sChangeEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ekglue_k8s_events",
			Help: "A count of events that Kubernetes notified us of.",
		},
		[]string{"event", "op"},
	)
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
	// TODO(jrockway): Eventually this "override" will have to be some sort of OverrideAction
	// object that allows suppresing the cluster entirely, etc.
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

// ClusterConfig configures creation of Envoy clusters from Kubernetes services.
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

// Field specifies a value to be selected from a Kubernetes resource.
//
// A non-empty Literal will override any Label selector.
type Field struct {
	Literal     string `json:"literal"`      // Specify a literal string to use.
	Label       string `json:"label"`        // Select the value of the named label.
	UseHostname bool   `json:"use_hostname"` // If true, use the hostname as the value of the field.
}

// LocalityConfig configures how to determine the locality of an endpoint.
type LocalityConfig struct {
	RegionFrom  *Field `json:"region_from"`
	ZoneFrom    *Field `json:"zone_from"`
	SubZoneFrom *Field `json:"sub_zone_from"`
}

// EndpointConfig configures creation of Envoy cluster load assignments from Kubernetes endpoints.
type EndpointConfig struct {
	IncludeNotReady bool            `json:"include_not_ready"`
	Locality        *LocalityConfig `json:"locality"`
}

// Config configures how to turn k8s resources into Envoy Clusters and ClusterLoadAssignments.
type Config struct {
	// The API version of this config file; not related to the Envoy dataplane API version.
	ApiVersion string `json:"apiVersion"`
	// Configuration for converting services to clusters.
	ClusterConfig *ClusterConfig `json:"cluster_config"`
	// Configuration for converting endpoints to cluster load assignments.
	EndpointConfig *EndpointConfig `json:"endpoint_config"`
}

func DefaultConfig() *Config {
	return &Config{
		ClusterConfig: &ClusterConfig{
			BaseConfig: &envoy_api_v2.Cluster{
				ConnectTimeout: ptypes.DurationProto(time.Second),
			},
		},
		EndpointConfig: &EndpointConfig{
			Locality: &LocalityConfig{},
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
	// TODO(jrockway): Future versions of this code will have to first unmarshal into a
	// temporary structure just to read the ApiVersion, then call version-specific unmarshalling
	// code based on this value.
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

func singleTargetLoadAssignment(cluster, hostname string, port int32, proto envoy_api_v2_core.SocketAddress_Protocol) *envoy_api_v2.ClusterLoadAssignment {
	return &envoy_api_v2.ClusterLoadAssignment{
		ClusterName: cluster,
		Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{{
			LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
				lbEndpoint(hostname, port, proto, envoy_api_v2_core.HealthStatus_UNKNOWN)},
		}},
	}
}

func lbEndpoint(hostname string, port int32, proto envoy_api_v2_core.SocketAddress_Protocol, health envoy_api_v2_core.HealthStatus) *envoy_api_v2_endpoint.LbEndpoint {
	return &envoy_api_v2_endpoint.LbEndpoint{
		HealthStatus: health,
		HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
			Endpoint: &envoy_api_v2_endpoint.Endpoint{
				Address: &envoy_api_v2_core.Address{
					Address: &envoy_api_v2_core.Address_SocketAddress{
						SocketAddress: &envoy_api_v2_core.SocketAddress{
							Protocol: proto,
							Address:  hostname,
							PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
								PortValue: uint32(port),
							},
						},
					},
				},
			},
		},
	}
}

func (c *ClusterConfig) isEDS(cl *envoy_api_v2.Cluster) bool {
	dtype := cl.GetClusterDiscoveryType()
	if dtype == nil {
		return false
	}
	return cl.GetType() == envoy_api_v2.Cluster_EDS
}

// nameCluster maps a port object from a service or endpoint to a name.  For EDS, the cluster and
// endpoint have to map to the same name, which is why we do this in one place.  It is imperfect,
// however, because you can have endpoints without services, and we never create a cluster for
// those.  We also return the Envoy protocol of the port here, because it's convenient, not because
// it's good design.
func nameCluster(namespace, serviceOrEndpoint, portName string, portNumber int32, portProtocol v1.Protocol) (string, envoy_api_v2_core.SocketAddress_Protocol) {
	var protoSuffix string
	var envoyProtocol envoy_api_v2_core.SocketAddress_Protocol
	switch portProtocol {
	case v1.ProtocolTCP, "":
		protoSuffix = ""
		envoyProtocol = envoy_api_v2_core.SocketAddress_TCP
	case v1.ProtocolUDP:
		protoSuffix = ":udp"
		envoyProtocol = envoy_api_v2_core.SocketAddress_UDP
	case v1.ProtocolSCTP:
		// Envoy doesn't support SCTP, so neither do we.  See Envoy issue
		// https://github.com/envoyproxy/envoy/issues/9430
		fallthrough
	default:
		return "", 0
	}
	if portName == "" {
		portName = strconv.Itoa(int(portNumber))
	}
	return fmt.Sprintf("%s:%s:%s%s", namespace, serviceOrEndpoint, portName, protoSuffix), envoyProtocol
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
		var protocol envoy_api_v2_core.SocketAddress_Protocol
		cl.Name, protocol = nameCluster(svc.GetNamespace(), svc.GetName(), port.Name, port.Port, port.Protocol)
		if cl.Name == "" {
			// Ignore clusters that we can't name, probably because they use an unsupported protcol.
			continue
		}
		proto.Merge(cl, c.GetOverride(cl, svc, port))
		if !c.isEDS(cl) {
			if cl.ClusterDiscoveryType == nil {
				cl.ClusterDiscoveryType = &envoy_api_v2.Cluster_Type{
					Type: envoy_api_v2.Cluster_STRICT_DNS,
				}
			}
			cl.LoadAssignment = singleTargetLoadAssignment(cl.Name, fmt.Sprintf("%s.%s.svc.cluster.local.", svc.GetName(), svc.GetNamespace()), port.Port, protocol)
		}
		result = append(result, cl)
	}
	return result
}

// extractLabel extracts a label from a node.
func extractLabel(node *v1.Node, hostname string, rule *Field) string {
	if rule == nil {
		return ""
	}
	if rule.Literal != "" {
		return rule.Literal
	}
	if rule.UseHostname {
		return hostname
	}
	if node == nil {
		return ""
	}
	labels := node.GetLabels()
	return labels[rule.Label]
}

// LocalityFromHost returns a locality record for the provided host.  nil is returned if there is no
// possible way to generate a locality.
func (l *LocalityConfig) LocalityFromHost(hosts cache.Store, hostname string) *envoy_api_v2_core.Locality {
	if l == nil || l.RegionFrom == nil && l.ZoneFrom == nil && l.SubZoneFrom == nil {
		return nil
	}

	result := new(envoy_api_v2_core.Locality)
	var node *v1.Node
	if hosts != nil {
		obj, exists, err := hosts.GetByKey(hostname)
		if err != nil {
			zap.L().Error("problem looking up node by hostname", zap.String("hostname", hostname), zap.Error(err))
		}
		if host, ok := obj.(*v1.Node); ok && exists {
			node = host
		}
	}
	if l != nil {
		if l.RegionFrom != nil {
			result.Region = extractLabel(node, hostname, l.RegionFrom)
		}
		if l.ZoneFrom != nil {
			result.Zone = extractLabel(node, hostname, l.ZoneFrom)
		}
		if l.SubZoneFrom != nil {
			result.SubZone = extractLabel(node, hostname, l.SubZoneFrom)
		}
	}
	return result
}

// LoadAssignmentFromEndpoints translates a Kubernetes endpoints object into a set of Envoy
// ClusterLoadAssignments.
func (c *EndpointConfig) LoadAssignmentsFromEndpoints(eps *v1.Endpoints) []*envoy_api_v2.ClusterLoadAssignment {
	if eps == nil {
		return nil
	}
	endpointsByClusterByHost := make(map[string]map[string][]*envoy_api_v2_endpoint.LbEndpoint)
	addEndpoint := func(addr v1.EndpointAddress, cluster string, port int32, protocol envoy_api_v2_core.SocketAddress_Protocol, health envoy_api_v2_core.HealthStatus) {
		host := ""
		if addr.NodeName != nil {
			host = *addr.NodeName
		}
		endpointsByHost, ok := endpointsByClusterByHost[cluster]
		if !ok {
			endpointsByHost = make(map[string][]*envoy_api_v2_endpoint.LbEndpoint)
			endpointsByClusterByHost[cluster] = endpointsByHost
		}
		endpointsByHost[host] = append(endpointsByHost[host], lbEndpoint(addr.IP, port, protocol, health))
	}

	for _, ss := range eps.Subsets {
		for _, port := range ss.Ports {
			cluster, protocol := nameCluster(eps.GetNamespace(), eps.GetName(), port.Name, port.Port, port.Protocol)
			if cluster == "" {
				// Ignore clusters that we can't name, probably because they use an
				// unsupported protocol.
				continue
			}
			for _, addr := range ss.Addresses {
				addEndpoint(addr, cluster, port.Port, protocol, envoy_api_v2_core.HealthStatus_HEALTHY)
			}
			if c.IncludeNotReady {
				for _, addr := range ss.NotReadyAddresses {
					addEndpoint(addr, cluster, port.Port, protocol, envoy_api_v2_core.HealthStatus_DEGRADED)
				}
			}
		}
	}

	var result []*envoy_api_v2.ClusterLoadAssignment
	for cluster, endpointsByHost := range endpointsByClusterByHost {
		var localityEndpoints []*envoy_api_v2_endpoint.LocalityLbEndpoints
		for host, endpoints := range endpointsByHost {
			locality := c.Locality.LocalityFromHost(nil, host)
			sort.Slice(endpoints, func(i, j int) bool {
				return endpoints[i].String() < endpoints[j].String()
			})
			localityEndpoints = append(localityEndpoints, &envoy_api_v2_endpoint.LocalityLbEndpoints{
				Locality:    locality,
				LbEndpoints: endpoints,
			})
		}
		sort.Slice(localityEndpoints, func(i, j int) bool {
			return localityEndpoints[i].Locality.String() < localityEndpoints[j].Locality.String()
		})
		result = append(result, &envoy_api_v2.ClusterLoadAssignment{
			ClusterName: cluster,
			Endpoints:   localityEndpoints,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].GetClusterName() < result[j].GetClusterName()
	})
	return result
}

// ClusterStore is a cache.Store that receives updates about the status of Kubernetes services,
// translates the services to Envoy cluster objects with the provided config, and reports those
// clusters to the xDS server.
type ClusterStore struct {
	cfg *ClusterConfig
	s   *cds.Server
}

func startOp(opSource, opName string) (context.Context, func()) {
	k8sChangeEvents.WithLabelValues(opSource, opName).Inc()
	Logger.Debug("start reflector op", zap.String("reflector", opSource), zap.String("event", opName))
	// 10 seconds is hardcoded as the timeout because under normal circumstances, this will be
	// instantaneous.  xDS notifications only block if the stream event loop is blocked; it
	// doesn't block on waiting for Envoy to ACK/NACK.
	tctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	span := opentracing.StartSpan(fmt.Sprintf("reflector.%s.%s", opSource, opName))
	ctx := opentracing.ContextWithSpan(tctx, span)

	return ctx, func() {
		c()
		span.Finish()
	}
}

func logError(ctx context.Context) {
	span := opentracing.SpanFromContext(ctx)
	if span == nil {
		return
	}
	ext.Error.Set(span, true)
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
	ctx, c := startOp("services", "add")
	defer c()
	svc, ok := obj.(*v1.Service)
	if !ok {
		logError(ctx)
		return fmt.Errorf("add service: got non-service object %#v", obj)
	}
	if err := cs.s.AddClusters(ctx, cs.cfg.ClustersFromService(svc)); err != nil {
		logError(ctx)
		return fmt.Errorf("add service: clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Update(obj interface{}) error {
	ctx, c := startOp("services", "update")
	defer c()
	svc, ok := obj.(*v1.Service)
	if !ok {
		logError(ctx)
		return fmt.Errorf("update service: got non-service object %#v", obj)
	}
	if err := cs.s.AddClusters(ctx, cs.cfg.ClustersFromService(svc)); err != nil {
		logError(ctx)
		return fmt.Errorf("update service: add clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Delete(obj interface{}) error {
	ctx, c := startOp("services", "delete")
	defer c()
	svc, ok := obj.(*v1.Service)
	if !ok {
		logError(ctx)
		return fmt.Errorf("delete service: got non-service object %#v", obj)
	}
	clusters := cs.cfg.ClustersFromService(svc)
	for _, c := range clusters {
		cs.s.DeleteCluster(ctx, c.GetName())
	}
	return nil
}
func (cs *ClusterStore) List() []interface{} {
	Logger.Debug("List cluster")
	clusters := cs.s.ListClusters()
	var result []interface{}
	for _, c := range clusters {
		result = append(result, c)
	}
	return result
}

func (cs *ClusterStore) ListKeys() []string {
	Logger.Debug("ListKeys cluster")
	clusters := cs.s.ListClusters()
	var result []string
	for _, c := range clusters {
		result = append(result, c.GetName())
	}
	return result
}
func (cs *ClusterStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	Logger.Debug("Get cluster")
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}
func (cs *ClusterStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	Logger.Debug("GetByKey cluster")
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}
func (cs *ClusterStore) Replace(objs []interface{}, _ string) error {
	ctx, c := startOp("services", "replace")
	defer c()
	var clusters []*envoy_api_v2.Cluster
	for _, obj := range objs {
		svc, ok := obj.(*v1.Service)
		if !ok {
			logError(ctx)
			return fmt.Errorf("replace services: got non-service object %#v", obj)
		}
		clusters = append(clusters, cs.cfg.ClustersFromService(svc)...)
	}
	if err := cs.s.ReplaceClusters(ctx, clusters); err != nil {
		logError(ctx)
		return fmt.Errorf("replace services: replace clusters: %w", err)
	}
	return nil
}
func (cs *ClusterStore) Resync() error {
	// Nothing to do.
	return nil
}

// EndpointStore is a cache.Store that receives endpoints and converts them to
// ClusterLoadAssignment objects for EDS.
type EndpointStore struct {
	cfg *EndpointConfig
	s   *cds.Server
}

// Store returns a cache.Store that allows a Kubernetes reflector to sync endpoint changes to a CDS
// server.
func (c *EndpointConfig) Store(s *cds.Server) *EndpointStore {
	return &EndpointStore{
		cfg: c,
		s:   s,
	}
}

func (es *EndpointStore) Add(obj interface{}) error {
	ctx, c := startOp("endpoints", "add")
	defer c()
	eps, ok := obj.(*v1.Endpoints)
	if !ok {
		logError(ctx)
		return fmt.Errorf("add endpoints: got non-endpoints object: %#v", obj)
	}
	if err := es.s.AddEndpoints(ctx, es.cfg.LoadAssignmentsFromEndpoints(eps)); err != nil {
		logError(ctx)
		return fmt.Errorf("add endpoints: %v", err)
	}
	return nil
}
func (es *EndpointStore) Update(obj interface{}) error {
	ctx, c := startOp("endpoints", "update")
	defer c()
	eps, ok := obj.(*v1.Endpoints)
	if !ok {
		logError(ctx)
		return fmt.Errorf("update endpoints: got non-endpoints object: %#v", obj)
	}
	if err := es.s.AddEndpoints(ctx, es.cfg.LoadAssignmentsFromEndpoints(eps)); err != nil {
		logError(ctx)
		return fmt.Errorf("update endpoints: %v", err)
	}
	return nil
}
func (es *EndpointStore) Delete(obj interface{}) error {
	ctx, c := startOp("endpoints", "update")
	defer c()
	eps, ok := obj.(*v1.Endpoints)
	if !ok {
		logError(ctx)
		return fmt.Errorf("delete endpoints: got non-endpoints object: %#v", obj)
	}
	as := es.cfg.LoadAssignmentsFromEndpoints(eps)
	for _, a := range as {
		es.s.DeleteEndpoints(ctx, a.GetClusterName())
	}
	return nil
}
func (es *EndpointStore) List() []interface{} {
	Logger.Debug("List endpoints")
	return nil
}

func (es *EndpointStore) ListKeys() []string {
	Logger.Debug("ListKeys endpoints")
	return nil
}
func (es *EndpointStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	Logger.Debug("Get endpoints")
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}
func (es *EndpointStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	Logger.Debug("GetByKey endpoints")
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}
func (es *EndpointStore) Replace(objs []interface{}, _ string) error {
	ctx, c := startOp("endpoints", "replace")
	defer c()
	var as []*envoy_api_v2.ClusterLoadAssignment
	for _, obj := range objs {
		eps, ok := obj.(*v1.Endpoints)
		if !ok {
			logError(ctx)
			return fmt.Errorf("replace endpoints: got non-endpoints object: %#v", obj)
		}
		as = append(as, es.cfg.LoadAssignmentsFromEndpoints(eps)...)
	}
	if err := es.s.ReplaceEndpoints(ctx, as); err != nil {
		logError(ctx)
		return fmt.Errorf("replace endpoints: %v", err)
	}
	return nil
}
func (es *EndpointStore) Resync() error {
	// Nothing to do.
	return nil
}
