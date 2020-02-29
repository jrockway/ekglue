package glue

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

func TestClustersFromService(t *testing.T) {
	testData := []struct {
		name    string
		service *v1.Service
		want    []*envoy_api_v2.Cluster
	}{
		{
			name: "no services",
		},
		{
			name: "named port without override",
			service: &v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bar",
					Namespace: "foo",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name: "http",
							Port: 80,
						},
					},
				},
			},
			want: []*envoy_api_v2.Cluster{
				{
					Name:                 "foo:bar:http",
					ConnectTimeout:       ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80, envoy_api_v2_core.SocketAddress_TCP),
				},
			},
		},
		{
			name: "two ports",
			service: &v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bar",
					Namespace: "foo",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name: "http",
							Port: 80,
						},
						{
							Port: 443,
						},
					},
				},
			},
			want: []*envoy_api_v2.Cluster{
				{
					Name:                 "foo:bar:http",
					ConnectTimeout:       ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80, envoy_api_v2_core.SocketAddress_TCP),
				},
				{
					Name:                 "foo:bar:443",
					ConnectTimeout:       ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:443", "bar.foo.svc.cluster.local.", 443, envoy_api_v2_core.SocketAddress_TCP),
				},
			},
		},
		{
			name: "named port with override",
			service: &v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bar",
					Namespace: "foo",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name: "http2",
							Port: 80,
						},
					},
				},
			},
			want: []*envoy_api_v2.Cluster{
				{
					Name:                 "foo:bar:http2",
					ConnectTimeout:       ptypes.DurationProto(2 * time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS},
					LbPolicy:             envoy_api_v2.Cluster_RANDOM,
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http2", "bar.foo.svc.cluster.local.", 80, envoy_api_v2_core.SocketAddress_TCP),
					Http2ProtocolOptions: &envoy_api_v2_core.Http2ProtocolOptions{},
					HealthChecks: []*envoy_api_v2_core.HealthCheck{
						{
							Timeout:  ptypes.DurationProto(time.Second),
							Interval: ptypes.DurationProto(10 * time.Second),
							HealthyThreshold: &wrappers.UInt32Value{
								Value: 1,
							},
							UnhealthyThreshold: &wrappers.UInt32Value{
								Value: 2,
							},
							HealthChecker: &envoy_api_v2_core.HealthCheck_HttpHealthCheck_{
								HttpHealthCheck: &envoy_api_v2_core.HealthCheck_HttpHealthCheck{
									Host:            "test",
									Path:            "/healthz",
									CodecClientType: envoy_type.CodecClientType_HTTP2,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "cluster with EDS discovery",
			service: &v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "eds",
					Namespace: "foo",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name: "http",
							Port: 80,
						},
					},
				},
			},
			want: []*envoy_api_v2.Cluster{
				{
					Name:           "foo:eds:http",
					ConnectTimeout: ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{
						Type: envoy_api_v2.Cluster_EDS,
					},
					EdsClusterConfig: &envoy_api_v2.Cluster_EdsClusterConfig{
						EdsConfig: &envoy_api_v2_core.ConfigSource{
							ConfigSourceSpecifier: &envoy_api_v2_core.ConfigSource_ApiConfigSource{
								ApiConfigSource: &envoy_api_v2_core.ApiConfigSource{
									ApiType:             envoy_api_v2_core.ApiConfigSource_GRPC,
									TransportApiVersion: envoy_api_v2_core.ApiVersion_V2,
									GrpcServices: []*envoy_api_v2_core.GrpcService{{
										TargetSpecifier: &envoy_api_v2_core.GrpcService_EnvoyGrpc_{
											EnvoyGrpc: &envoy_api_v2_core.GrpcService_EnvoyGrpc{
												ClusterName: "xds",
											},
										},
									}},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg, err := LoadConfig("testdata/clusters_from_service_test.yaml")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			got := cfg.ClusterConfig.ClustersFromService(test.service)
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			sort.Slice(test.want, func(i, j int) bool { return test.want[i].Name < test.want[j].Name })
			if diff := cmp.Diff(got, test.want); diff != "" {
				t.Errorf("clusters:\n  got: %v\n want: %v\n diff: %v", got, test.want, diff)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	testData := []struct {
		name    string
		input   string
		want    *Config
		wantErr bool
	}{
		{
			name:  "valid config",
			input: "testdata/goodconfig.yaml",
			want: &Config{
				ApiVersion: "v1alpha",
				ClusterConfig: &ClusterConfig{
					BaseConfig: &envoy_api_v2.Cluster{
						ConnectTimeout: ptypes.DurationProto(2 * time.Second),
					},
					Overrides: []*ClusterOverride{
						{
							Match: []*Matcher{
								{
									ClusterName: "foo:bar:h2",
								},
								{
									ClusterName: "foo:baz:h2",
								},
							},
							Override: &envoy_api_v2.Cluster{
								Http2ProtocolOptions: &envoy_api_v2_core.Http2ProtocolOptions{},
							},
						},
					},
				},
				EndpointConfig: &EndpointConfig{
					IncludeNotReady: false,
					Locality: &LocalityConfig{
						RegionFrom:  &Field{Literal: "tests"},
						ZoneFrom:    &Field{Label: "$host"},
						SubZoneFrom: &Field{Label: "$host"},
					},
				},
			},
		},
		{
			name:    "bad apiVersion",
			input:   "testdata/badversion.yaml",
			wantErr: true,
		},
		{
			name:    "bad cluster",
			input:   "testdata/badcluster.yaml",
			wantErr: true,
		},
	}
	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			got, err := LoadConfig(test.input)
			if err != nil && !test.wantErr {
				t.Fatal(err)
			}
			if err == nil && test.wantErr {
				t.Fatal("expected error, but got success")
			}
			want := test.want
			if diff := cmp.Diff(got, want); diff != "" {
				t.Errorf("loaded yaml:\n  got: %#v\n want: %#v\n diff: %v", got, want, diff)
			}
		})
	}
}

func TestLoadAssignmentFromEndpoints(t *testing.T) {
	node0 := "host0"
	node1 := "host1"
	testData := []struct {
		name      string
		endpoints *v1.Endpoints
		want      []*envoy_api_v2.ClusterLoadAssignment
	}{
		{
			name:      "nil",
			endpoints: nil,
			want:      nil,
		},
		{
			name:      "empty",
			endpoints: &v1.Endpoints{},
			want:      nil,
		},
		{
			name: "ready_and_notready",
			endpoints: &v1.Endpoints{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Endpoints",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "foo",
					Name:      "bar",
				},
				Subsets: []v1.EndpointSubset{
					{
						Ports: []v1.EndpointPort{
							{
								Name:     "port",
								Port:     1234,
								Protocol: v1.ProtocolTCP,
							},
							{
								Name:     "debug",
								Port:     8080,
								Protocol: v1.ProtocolTCP,
							},
							{
								Name:     "udp",
								Port:     1234,
								Protocol: v1.ProtocolUDP,
							},
						},
						Addresses: []v1.EndpointAddress{
							{
								NodeName: &node0,
								IP:       "10.0.0.1",
							},
						},
						NotReadyAddresses: []v1.EndpointAddress{
							{
								NodeName: &node0,
								IP:       "10.0.0.2",
							},
						},
					},
					{
						Ports: []v1.EndpointPort{
							{
								Name:     "port",
								Port:     1234,
								Protocol: v1.ProtocolTCP,
							},
						},
						Addresses: []v1.EndpointAddress{
							{
								NodeName: &node1,
								IP:       "10.0.0.3",
							},
						},
						NotReadyAddresses: []v1.EndpointAddress{
							{
								NodeName: &node1,
								IP:       "10.0.0.4",
							},
						},
					},
				},
			},
			want: []*envoy_api_v2.ClusterLoadAssignment{
				{
					ClusterName: "foo:bar:debug",
					Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{
						{
							Locality: &envoy_api_v2_core.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
								lbEndpoint("10.0.0.1", 8080, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 8080, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_DEGRADED),
							},
						},
					},
				},
				{
					ClusterName: "foo:bar:port",
					Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{
						{
							Locality: &envoy_api_v2_core.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
								lbEndpoint("10.0.0.1", 1234, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 1234, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_DEGRADED),
							},
						},
						{
							Locality: &envoy_api_v2_core.Locality{
								Region:  "region0",
								Zone:    "host1",
								SubZone: "host1",
							},
							LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
								lbEndpoint("10.0.0.3", 1234, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.4", 1234, envoy_api_v2_core.SocketAddress_TCP, envoy_api_v2_core.HealthStatus_DEGRADED),
							},
						},
					},
				},
				{
					ClusterName: "foo:bar:udp:udp",
					Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{
						{
							Locality: &envoy_api_v2_core.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
								lbEndpoint("10.0.0.1", 1234, envoy_api_v2_core.SocketAddress_UDP, envoy_api_v2_core.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 1234, envoy_api_v2_core.SocketAddress_UDP, envoy_api_v2_core.HealthStatus_DEGRADED),
							},
						},
					},
				},
			},
		},
	}

	cfg := &Config{
		EndpointConfig: &EndpointConfig{
			IncludeNotReady: true,
			Locality: &LocalityConfig{
				RegionFrom: &Field{
					Label: "topology.kubernetes.io/region",
				},
				ZoneFrom: &Field{
					UseHostname: true,
				},
				SubZoneFrom: &Field{
					UseHostname: true,
				},
			},
		},
	}
	nodes := cache.NewStore(cache.MetaNamespaceKeyFunc)
	nodes.Add(&v1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "host0",
			Labels: map[string]string{
				"topology.kubernetes.io/region":            "region0",
				"topology.kubernetes.io/zone":              "region0-zone0",
				"failure-domain.beta.kubernetes.io/region": "region0",
				"failure-domain.beta.kubernetes.io/zone":   "region0-zone0",
			},
		},
	})
	nodes.Add(&v1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "host1",
			Labels: map[string]string{
				"topology.kubernetes.io/region":            "region0",
				"topology.kubernetes.io/zone":              "region0-zone0",
				"failure-domain.beta.kubernetes.io/region": "region0",
				"failure-domain.beta.kubernetes.io/zone":   "region0-zone0",
			},
		},
	})

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			got := cfg.EndpointConfig.LoadAssignmentsFromEndpoints(nodes, test.endpoints)
			if diff := cmp.Diff(got, test.want); diff != "" {
				t.Errorf("endpoints:\n  got: %v\n want: %v\n diff: %v", got, test.want, diff)
			}
		})
	}
}

func TestLocality(t *testing.T) {
	testData := []struct {
		localityConfig *LocalityConfig
		input          string
		want           *envoy_api_v2_core.Locality
	}{
		{
			localityConfig: nil,
			input:          "host0",
			want:           nil,
		},
		{
			localityConfig: &LocalityConfig{},
			input:          "host0",
			want:           nil,
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Literal: "region",
				},
			},
			input: "host0",
			want: &envoy_api_v2_core.Locality{
				Region:  "region",
				Zone:    "",
				SubZone: "",
			},
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Label: "topology.kubernetes.io/region",
				},
			},
			input: "host0",
			want: &envoy_api_v2_core.Locality{
				Region:  "region0",
				Zone:    "",
				SubZone: "",
			},
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Label: "topology.kubernetes.io/region",
				},
			},
			input: "host2",
			want: &envoy_api_v2_core.Locality{
				Region:  "",
				Zone:    "",
				SubZone: "",
			},
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Label: "topology.kubernetes.io/region",
				},
				ZoneFrom: &Field{
					Label: "topology.kubernetes.io/zone",
				},
				SubZoneFrom: &Field{
					UseHostname: true,
				},
			},
			input: "host0",
			want: &envoy_api_v2_core.Locality{
				Region:  "region0",
				Zone:    "region0-zone0",
				SubZone: "host0",
			},
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Label: "topology.kubernetes.io/region",
				},
				ZoneFrom: &Field{
					Label: "topology.kubernetes.io/zone",
				},
				SubZoneFrom: &Field{
					UseHostname: true,
				},
			},
			input: "host2",
			want: &envoy_api_v2_core.Locality{
				Region:  "",
				Zone:    "",
				SubZone: "host2",
			},
		},
	}

	nodes := cache.NewStore(cache.MetaNamespaceKeyFunc)
	nodes.Add(&v1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "host0",
			Labels: map[string]string{
				"topology.kubernetes.io/region":            "region0",
				"topology.kubernetes.io/zone":              "region0-zone0",
				"failure-domain.beta.kubernetes.io/region": "region0",
				"failure-domain.beta.kubernetes.io/zone":   "region0-zone0",
			},
		},
	})

	for i, test := range testData {
		got := test.localityConfig.LocalityFromHost(nodes, test.input)
		if want := test.want; !proto.Equal(got, want) {
			t.Errorf("test %d: locality:\n  got: %#v\n want: %#v", i, got, want)
		}
	}
}

func TestLocalitiesAsYAML(t *testing.T) {
	s := cache.NewStore(cache.MetaNamespaceKeyFunc)
	s.Add(&v1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "host0",
			Labels: map[string]string{
				"topology.kubernetes.io/region":            "region0",
				"topology.kubernetes.io/zone":              "region0-zone0",
				"failure-domain.beta.kubernetes.io/region": "region0",
				"failure-domain.beta.kubernetes.io/zone":   "region0-zone0",
			},
		},
	})
	l := &LocalityConfig{
		RegionFrom: &Field{
			Label: "topology.kubernetes.io/region",
		},
		ZoneFrom: &Field{
			Label: "topology.kubernetes.io/zone",
		},
		SubZoneFrom: &Field{
			UseHostname: true,
		},
	}

	locBytes, err := l.LocalitiesAsYAML(s)
	if err != nil {
		t.Fatal(err)
	}

	locJSON, err := yaml.YAMLToJSON(locBytes)
	if err != nil {
		t.Fatal(err)
	}
	nl := &nodeLocalities{Localities: make(map[string]json.RawMessage)}
	if err := json.Unmarshal(locJSON, nl); err != nil {
		t.Fatal(err)
	}
	if got, want := len(nl.Localities), 1; got != want {
		t.Errorf("host count:\n  got: %v\n want: %v", got, want)
	}
}
