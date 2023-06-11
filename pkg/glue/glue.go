// Package glue glues Kubernetes to Envoy
package glue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"golang.org/x/exp/maps"

	// for config loading
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"

	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

var (
	// Logger is the default logger for glue events; for extreme debugging, you can overwrite this.
	Logger = zap.NewNop()

	k8sChangeEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ekglue_k8s_events",
			Help: "A count of events that Kubernetes notified us of.",
		},
		[]string{"event", "op"},
	)
)

// A matcher selects a cluster based on the current state of the generated Cluster object, the and
// Kubernetes service + port that the Cluster is being created for.
type Matcher struct {
	// ClusterName matches the mangled name of a cluster (not the original service name).
	//
	// The mangled name is <namespace>:<service name>:<port name or number>[:udp].
	ClusterName string `json:"cluster_name"`
	// PortName matches the name of a port.  This exists so that if you have good port naming
	// hygiene, more configurations can be auto-generated.  For example, you could apply
	// "http2_protocol_options: {}" to all ports named http2.
	//
	// You cannot match an unnamed port with an empty port_name.
	PortName string `json:"port_name"`
}

// Evaluate returns true if the matcher matches the provided objects.
func (m *Matcher) Evaluate(cluster *envoy_config_cluster_v3.Cluster, svc *v1.Service, port *v1.ServicePort) bool {
	if m == nil {
		return false
	}
	if port == nil {
		return false
	}
	if m.ClusterName != "" {
		return m.ClusterName == cluster.GetName()
	}
	if m.PortName != "" {
		return m.PortName == port.Name
	}
	return false
}

type ClusterOverride struct {
	// Match specifies a cluster to match; multiple items are OR'd.
	Match []*Matcher
	// Configuration to override if a matcher matches.
	Override *envoy_config_cluster_v3.Cluster
	// If true, suppress the cluster completely.
	Suppress bool
}

func (o *ClusterOverride) UnmarshalJSON(b []byte) error {
	tmp := struct {
		Match    []*Matcher      `json:"match"`
		Override json.RawMessage `json:"override"`
		Suppress bool            `json:"suppress"`
	}{}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return fmt.Errorf("ClusterOverride: unmarshal into temporary structure: %w", err)
	}
	o.Match = tmp.Match
	o.Suppress = tmp.Suppress
	if len(tmp.Override) > 0 {
		base := &envoy_config_cluster_v3.Cluster{}
		if err := protojson.Unmarshal(tmp.Override, base); err != nil {
			return fmt.Errorf("ClusterOverride: unmarshal Override: %w", err)
		}
		o.Override = base
	}
	if len(o.Match) == 0 {
		return fmt.Errorf("ClusterOverride: no matching rules provided")
	}
	if o.Override != nil && o.Suppress {
		return fmt.Errorf("ClusterOverride: expected exactly one of [override, suppress], but got both")
	}
	if o.Override == nil && !o.Suppress {
		return fmt.Errorf("ClusterOverride: expected exactly one of [override, suppress], but got neither")
	}
	return nil
}

// ClusterConfig configures creation of Envoy clusters from Kubernetes services.
type ClusterConfig struct {
	// The base configuration that should be used for all clusters.
	BaseConfig *envoy_config_cluster_v3.Cluster `json:"base"`
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

	base := &envoy_config_cluster_v3.Cluster{}
	if err := protojson.Unmarshal(tmp.BaseConfig, base); err != nil {
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
	APIVersion string `json:"apiVersion"`
	// Configuration for converting services to clusters.
	ClusterConfig *ClusterConfig `json:"cluster_config"`
	// Configuration for converting endpoints to cluster load assignments.
	EndpointConfig *EndpointConfig `json:"endpoint_config"`
}

func DefaultConfig() *Config {
	return &Config{
		ClusterConfig: &ClusterConfig{
			BaseConfig: &envoy_config_cluster_v3.Cluster{
				ConnectTimeout: durationpb.New(time.Second),
			},
		},
		EndpointConfig: &EndpointConfig{
			Locality: &LocalityConfig{},
		},
	}
}

func LoadConfig(filename string) (*Config, error) {
	raw, err := os.ReadFile(filename)
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
	// temporary structure just to read the APIVersion, then call version-specific unmarshalling
	// code based on this value.
	if v := cfg.APIVersion; v != "v1alpha" {
		return nil, fmt.Errorf("unknown config version %q; expected v1alpha", v)
	}
	return cfg, nil
}

// Base returns a deep copy of the base cluster configuration.
func (c *ClusterConfig) GetBaseConfig() *envoy_config_cluster_v3.Cluster {
	raw := proto.Clone(c.BaseConfig)
	cluster, ok := raw.(*envoy_config_cluster_v3.Cluster)
	if !ok {
		zap.L().Fatal("internal error: couldn't clone ClusterConfig.BaseConfig")
	}
	return cluster
}

// ApplyOverride returns the cluster after applying any configured overrides.  It will return nil if
// the cluster is suppressed.
func (c *ClusterConfig) ApplyOverride(cluster *envoy_config_cluster_v3.Cluster, svc *v1.Service, port *v1.ServicePort) *envoy_config_cluster_v3.Cluster {
	for _, o := range c.Overrides {
		var match bool
		for _, m := range o.Match {
			match = match || m.Evaluate(cluster, svc, port)
			if match {
				break
			}
		}
		if match {
			if o.Override != nil {
				proto.Merge(cluster, o.Override)
				continue
			}
			if o.Suppress {
				return nil
			}
		}
	}
	return cluster
}

func singleTargetLoadAssignment(cluster, hostname string, port int32, protocol envoy_config_core_v3.SocketAddress_Protocol) *envoy_config_endpoint_v3.ClusterLoadAssignment {
	return &envoy_config_endpoint_v3.ClusterLoadAssignment{
		ClusterName: cluster,
		Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{{
			LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
				lbEndpoint(hostname, port, protocol, envoy_config_core_v3.HealthStatus_UNKNOWN)},
		}},
	}
}

func lbEndpoint(hostname string, port int32, protocol envoy_config_core_v3.SocketAddress_Protocol, health envoy_config_core_v3.HealthStatus) *envoy_config_endpoint_v3.LbEndpoint {
	return &envoy_config_endpoint_v3.LbEndpoint{
		HealthStatus: health,
		HostIdentifier: &envoy_config_endpoint_v3.LbEndpoint_Endpoint{
			Endpoint: &envoy_config_endpoint_v3.Endpoint{
				Address: &envoy_config_core_v3.Address{
					Address: &envoy_config_core_v3.Address_SocketAddress{
						SocketAddress: &envoy_config_core_v3.SocketAddress{
							Protocol: protocol,
							Address:  hostname,
							PortSpecifier: &envoy_config_core_v3.SocketAddress_PortValue{
								PortValue: uint32(port),
							},
						},
					},
				},
			},
		},
	}
}

func (c *ClusterConfig) isEDS(cl *envoy_config_cluster_v3.Cluster) bool {
	dtype := cl.GetClusterDiscoveryType()
	if dtype == nil {
		return false
	}
	return cl.GetType() == envoy_config_cluster_v3.Cluster_EDS
}

// nameCluster maps a port object from a service or endpoint to a name.  For EDS, the cluster and
// endpoint have to map to the same name, which is why we do this in one place.  It is imperfect,
// however, because you can have endpoints without services, and we never create a cluster for
// those.  We also return the Envoy protocol of the port here, because it's convenient, not because
// it's good design.
func nameCluster(namespace, service, portName string, portNumber int32, portProtocol v1.Protocol) (string, envoy_config_core_v3.SocketAddress_Protocol) {
	var protoSuffix string
	var envoyProtocol envoy_config_core_v3.SocketAddress_Protocol
	switch portProtocol {
	case v1.ProtocolTCP, "":
		protoSuffix = ""
		envoyProtocol = envoy_config_core_v3.SocketAddress_TCP
	case v1.ProtocolUDP:
		protoSuffix = ":udp"
		envoyProtocol = envoy_config_core_v3.SocketAddress_UDP
	case v1.ProtocolSCTP:
		// Envoy doesn't support SCTP, so neither do we.  See Envoy issue
		// https://github.com/envoyproxy/envoy/issues/9430
		fallthrough //nolint
	default:
		return "", 0
	}
	if portName == "" {
		portName = strconv.Itoa(int(portNumber))
	}
	return fmt.Sprintf("%s:%s:%s%s", namespace, service, portName, protoSuffix), envoyProtocol
}

// ClustersFromService translates a Kubernetes service into a set of Envoy clusters according to the
// config (1 cluster per service port).
func (c *ClusterConfig) ClustersFromService(svc *v1.Service) []*envoy_config_cluster_v3.Cluster {
	var result []*envoy_config_cluster_v3.Cluster
	if svc == nil {
		return nil
	}
	for _, port := range svc.Spec.Ports {
		cl := c.GetBaseConfig()
		var protocol envoy_config_core_v3.SocketAddress_Protocol
		cl.Name, protocol = nameCluster(svc.GetNamespace(), svc.GetName(), port.Name, port.Port, port.Protocol)
		if cl.Name == "" {
			// Ignore clusters that we can't name, probably because they use an unsupported protcol.
			continue
		}
		cl = c.ApplyOverride(cl, svc, &port)
		if cl == nil {
			continue
		}
		if !c.isEDS(cl) {
			if cl.ClusterDiscoveryType == nil {
				cl.ClusterDiscoveryType = &envoy_config_cluster_v3.Cluster_Type{
					Type: envoy_config_cluster_v3.Cluster_STRICT_DNS,
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

// LocalityFromHost returns a locality record for the provided host, looking in the cache.Store for
// a v1.Node object that matches the hostname.  It returns an empty, non-nil, Locality if there is
// no way to determine the actual locality.
func (l *LocalityConfig) LocalityFromHost(hosts cache.Store, hostname string) *envoy_config_core_v3.Locality {
	result := new(envoy_config_core_v3.Locality)
	if l == nil || l.RegionFrom == nil && l.ZoneFrom == nil && l.SubZoneFrom == nil {
		return result
	}
	if hostname == "" {
		return result
	}
	var node *v1.Node
	if hosts != nil {
		obj, exists, err := hosts.GetByKey(hostname)
		if err != nil {
			zap.L().Error("problem looking up node by hostname", zap.String("hostname", hostname), zap.Error(err))
		} else if !exists {
			zap.L().Info("no match for hostname in node cache; cannot emit locality information", zap.String("hostname", hostname))
		}
		if host, ok := obj.(*v1.Node); ok && exists && host != nil {
			node = host
		}
	}

	if l.RegionFrom != nil {
		result.Region = extractLabel(node, hostname, l.RegionFrom)
	}
	if l.ZoneFrom != nil {
		result.Zone = extractLabel(node, hostname, l.ZoneFrom)
	}
	if l.SubZoneFrom != nil {
		result.SubZone = extractLabel(node, hostname, l.SubZoneFrom)
	}

	return result
}

type nodeLocalities struct {
	Localities map[string]json.RawMessage `json:"localities"`
}

// LocalitiesAsYAML returns a YAML string showing the configured locality for every node in the
// provided cache.Store.
func (l *LocalityConfig) LocalitiesAsYAML(nodes cache.Store) ([]byte, error) {
	localities := &nodeLocalities{Localities: make(map[string]json.RawMessage)}
	jsonm := &protojson.MarshalOptions{EmitUnpopulated: false}
	for _, obj := range nodes.List() {
		node, ok := obj.(*v1.Node)
		locality := &envoy_config_core_v3.Locality{}
		if ok {
			locality = l.LocalityFromHost(nodes, node.GetName())
		}
		locJSON, err := jsonm.Marshal(locality)
		if err != nil {
			return nil, fmt.Errorf("marshal json for node %s: %v", node.GetName(), err)
		}
		localities.Localities[node.GetName()] = json.RawMessage(locJSON)
	}
	localitiesJSON, err := json.Marshal(localities)
	if err != nil {
		return nil, fmt.Errorf("marshal localities: %v", err)
	}
	localitiesYAML, err := yaml.JSONToYAML([]byte(localitiesJSON))
	if err != nil {
		return nil, fmt.Errorf("convert json to yaml: %v", err)
	}
	return localitiesYAML, nil
}

// LoadAssignmentFromEndpoints translates a Kubernetes endpoints object into a set of Envoy
// ClusterLoadAssignments.
func (c *EndpointConfig) LoadAssignmentsFromEndpointSlices(nodeStore cache.Store, endpointSlices []*discoveryv1.EndpointSlice) []*envoy_config_endpoint_v3.ClusterLoadAssignment {
	if endpointSlices == nil {
		return nil
	}

	endpointsByClusterByNode := make(map[string]map[string][]*envoy_config_endpoint_v3.LbEndpoint)
	for _, es := range endpointSlices {
		svc := esService(es)
		for _, port := range es.Ports {
			if port.Port == nil {
				// Ignore unspecified ports.
				continue
			}
			portNum := *port.Port
			portName := withDefault(port.Name, "")
			portProto := withDefault(port.Protocol, "TCP")
			cluster, protocol := nameCluster(svc.Namespace, svc.Name, portName, portNum, portProto)
			if cluster == "" {
				// Ignore clusters that we can't name, probably because they use an unsupported protocol.
				continue
			}
			endpointsByNode, ok := endpointsByClusterByNode[cluster]
			if !ok {
				endpointsByNode = make(map[string][]*envoy_config_endpoint_v3.LbEndpoint)
				endpointsByClusterByNode[cluster] = endpointsByNode
			}
			for _, ep := range es.Endpoints {
				health := envoy_config_core_v3.HealthStatus_HEALTHY
				if !withDefault(ep.Conditions.Ready, true) {
					if !c.IncludeNotReady {
						continue
					}
					health = envoy_config_core_v3.HealthStatus_DEGRADED
				}
				node := withDefault(ep.NodeName, "")
				for _, addr := range ep.Addresses {
					endpointsByNode[node] = append(endpointsByNode[node], lbEndpoint(addr, portNum, protocol, health))
				}
			}
		}
	}

	var result []*envoy_config_endpoint_v3.ClusterLoadAssignment
	for cluster, endpointsByNode := range endpointsByClusterByNode {
		var localityEndpoints []*envoy_config_endpoint_v3.LocalityLbEndpoints
		for node, endpoints := range endpointsByNode {
			sort.Slice(endpoints, func(i, j int) bool {
				return endpoints[i].String() < endpoints[j].String()
			})
			localityEndpoints = append(localityEndpoints, &envoy_config_endpoint_v3.LocalityLbEndpoints{
				Locality:    c.Locality.LocalityFromHost(nodeStore, node),
				LbEndpoints: endpoints,
			})
		}
		sort.Slice(localityEndpoints, func(i, j int) bool {
			return localityEndpoints[i].Locality.String() < localityEndpoints[j].Locality.String()
		})
		result = append(result, &envoy_config_endpoint_v3.ClusterLoadAssignment{
			ClusterName: cluster,
			Endpoints:   localityEndpoints,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].GetClusterName() < result[j].GetClusterName()
	})
	return result
}

func clusterNames(slices map[string]*discoveryv1.EndpointSlice) map[string]struct{} {
	clusters := make(map[string]struct{})
	for _, eps := range slices {
		svc := esService(eps)
		for _, port := range eps.Ports {
			if port.Port == nil {
				// Ignore unspecified ports.
				continue
			}
			cluster, _ := nameCluster(svc.Namespace, svc.Name, withDefault(port.Name, ""), *port.Port, withDefault(port.Protocol, "TCP"))
			if cluster == "" {
				// Ignore clusters that we can't name, probably because they use an unsupported protocol.
				continue
			}
			clusters[cluster] = struct{}{}
		}
	}
	return clusters
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
	var clusters []*envoy_config_cluster_v3.Cluster
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
	cfg       *EndpointConfig
	srv       *cds.Server
	nodeStore cache.Store

	mu        sync.Mutex
	serverESs map[types.NamespacedName]map[string]*discoveryv1.EndpointSlice
}

// Store returns a cache.Store that allows a Kubernetes reflector to sync endpoint changes to an EDS
// server.
func (c *EndpointConfig) Store(nodeStore cache.Store, s *cds.Server) *EndpointStore {
	return &EndpointStore{
		cfg:       c,
		srv:       s,
		nodeStore: nodeStore,
		serverESs: make(map[types.NamespacedName]map[string]*discoveryv1.EndpointSlice),
	}
}

func (s *EndpointStore) Add(obj interface{}) error {
	return s.update("add", obj, func(svcESs map[string]*discoveryv1.EndpointSlice, es *discoveryv1.EndpointSlice) {
		svcESs[es.Name] = es
	})
}

func (s *EndpointStore) Update(obj interface{}) error {
	return s.update("update", obj, func(svcESs map[string]*discoveryv1.EndpointSlice, es *discoveryv1.EndpointSlice) {
		svcESs[es.Name] = es
	})
}

func (s *EndpointStore) Delete(obj interface{}) error {
	return s.update("delete", obj, func(svcESs map[string]*discoveryv1.EndpointSlice, es *discoveryv1.EndpointSlice) {
		delete(svcESs, es.Name)
	})
}

func (s *EndpointStore) update(op string, obj any, updateFn func(svcESs map[string]*discoveryv1.EndpointSlice, es *discoveryv1.EndpointSlice)) error {
	ctx, c := startOp("endpointslice", op)
	defer c()

	es, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		logError(ctx)
		return fmt.Errorf("%s endpointslice: got non-endpointslice object: %#v", op, obj)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	svc := esService(es)
	svcESs, ok := s.serverESs[svc]
	if !ok {
		svcESs = make(map[string]*discoveryv1.EndpointSlice)
		s.serverESs[svc] = svcESs
	}
	prevClusters := clusterNames(svcESs)
	updateFn(svcESs, es)
	loadAssignments := s.cfg.LoadAssignmentsFromEndpointSlices(s.nodeStore, maps.Values(svcESs))

	// Delete assignments for any clusters which no longer exist.
	for _, ep := range loadAssignments {
		delete(prevClusters, ep.ClusterName)
	}
	for cluster := range prevClusters {
		s.srv.DeleteEndpoints(ctx, cluster)
	}
	if len(svcESs) == 0 {
		delete(s.serverESs, svc)
	}

	// Set new assignments.
	if err := s.srv.AddEndpoints(ctx, loadAssignments); err != nil {
		logError(ctx)
		return fmt.Errorf("%s endpoints: %v", op, err)
	}
	return nil
}

func (s *EndpointStore) List() []interface{} {
	Logger.Debug("List endpoints")
	return nil
}

func (s *EndpointStore) ListKeys() []string {
	Logger.Debug("ListKeys endpoints")
	return nil
}

func (s *EndpointStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	Logger.Debug("Get endpoints")
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}

func (s *EndpointStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	Logger.Debug("GetByKey endpoints")
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}

func (s *EndpointStore) Replace(objs []interface{}, _ string) error {
	ctx, c := startOp("endpointslice", "replace")
	defer c()

	s.mu.Lock()
	defer s.mu.Unlock()

	var endpoints []*discoveryv1.EndpointSlice
	serviceEps := make(map[types.NamespacedName]map[string]*discoveryv1.EndpointSlice)
	for _, obj := range objs {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			logError(ctx)
			return fmt.Errorf("replace endpointslice: got non-endpointslice object: %#v", obj)
		}
		svc := esService(slice)
		svcESs, ok := serviceEps[svc]
		if !ok {
			svcESs = make(map[string]*discoveryv1.EndpointSlice)
			serviceEps[svc] = svcESs
		}
		svcESs[slice.Name] = slice
		endpoints = append(endpoints, slice)
	}
	loadAssignments := s.cfg.LoadAssignmentsFromEndpointSlices(s.nodeStore, endpoints)
	if err := s.srv.ReplaceEndpoints(ctx, loadAssignments); err != nil {
		logError(ctx)
		return fmt.Errorf("replace endpoints: %v", err)
	}
	s.serverESs = serviceEps
	return nil
}

func (s *EndpointStore) Resync() error {
	// Nothing to do.
	return nil
}

func esService(es *discoveryv1.EndpointSlice) types.NamespacedName {
	return types.NamespacedName{
		Namespace: es.Namespace,
		Name:      es.Labels[discoveryv1.LabelServiceName],
	}
}

func withDefault[T comparable](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
