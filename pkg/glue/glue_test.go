package glue

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoy_type_v3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/jrockway/ekglue/pkg/cds"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

func TestClustersFromService(t *testing.T) {
	testData := []struct {
		name    string
		service *v1.Service
		want    []*envoy_config_cluster_v3.Cluster
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
			want: []*envoy_config_cluster_v3.Cluster{
				{
					Name:                 "foo:bar:http",
					ConnectTimeout:       durationpb.New(time.Second),
					ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80, envoy_config_core_v3.SocketAddress_TCP),
				},
			},
		},
		{
			name: "unsupported sctp cluster",
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
							Name:     "sctp",
							Port:     80,
							Protocol: v1.ProtocolSCTP,
						},
					},
				},
			},
			want: nil,
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
			want: []*envoy_config_cluster_v3.Cluster{
				{
					Name:                 "foo:bar:http",
					ConnectTimeout:       durationpb.New(time.Second),
					ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80, envoy_config_core_v3.SocketAddress_TCP),
				},
				{
					Name:                 "foo:bar:443",
					ConnectTimeout:       durationpb.New(time.Second),
					ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:443", "bar.foo.svc.cluster.local.", 443, envoy_config_core_v3.SocketAddress_TCP),
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
			want: []*envoy_config_cluster_v3.Cluster{
				{
					Name:                 "foo:bar:http2",
					ConnectTimeout:       durationpb.New(2 * time.Second),
					ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_STRICT_DNS},
					LbPolicy:             envoy_config_cluster_v3.Cluster_RANDOM,
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http2", "bar.foo.svc.cluster.local.", 80, envoy_config_core_v3.SocketAddress_TCP),
					Http2ProtocolOptions: &envoy_config_core_v3.Http2ProtocolOptions{},
					HealthChecks: []*envoy_config_core_v3.HealthCheck{
						{
							Timeout:            durationpb.New(time.Second),
							Interval:           durationpb.New(10 * time.Second),
							HealthyThreshold:   wrapperspb.UInt32(1),
							UnhealthyThreshold: wrapperspb.UInt32(2),
							HealthChecker: &envoy_config_core_v3.HealthCheck_HttpHealthCheck_{
								HttpHealthCheck: &envoy_config_core_v3.HealthCheck_HttpHealthCheck{
									Host:            "test",
									Path:            "/healthz",
									CodecClientType: envoy_type_v3.CodecClientType_HTTP2,
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
			want: []*envoy_config_cluster_v3.Cluster{
				{
					Name:           "foo:eds:http",
					ConnectTimeout: durationpb.New(time.Second),
					ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{
						Type: envoy_config_cluster_v3.Cluster_EDS,
					},
					EdsClusterConfig: &envoy_config_cluster_v3.Cluster_EdsClusterConfig{
						EdsConfig: &envoy_config_core_v3.ConfigSource{
							ResourceApiVersion: envoy_config_core_v3.ApiVersion_V3,
							ConfigSourceSpecifier: &envoy_config_core_v3.ConfigSource_ApiConfigSource{
								ApiConfigSource: &envoy_config_core_v3.ApiConfigSource{
									ApiType:             envoy_config_core_v3.ApiConfigSource_GRPC,
									TransportApiVersion: envoy_config_core_v3.ApiVersion_V3,
									GrpcServices: []*envoy_config_core_v3.GrpcService{{
										TargetSpecifier: &envoy_config_core_v3.GrpcService_EnvoyGrpc_{
											EnvoyGrpc: &envoy_config_core_v3.GrpcService_EnvoyGrpc{
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
		{
			name: "suppressed cluster",
			service: &v1.Service{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Service",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "baz",
					Namespace: "foo",
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name: "http",
							Port: 80,
						},
						{
							Name: "https",
							Port: 443,
						},
					},
				},
			},
			want: nil,
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
			if diff := cmp.Diff(got, test.want, protocmp.Transform()); diff != "" {
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
				APIVersion: "v1alpha",
				ClusterConfig: &ClusterConfig{
					BaseConfig: &envoy_config_cluster_v3.Cluster{
						ConnectTimeout: durationpb.New(2 * time.Second),
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
							Override: &envoy_config_cluster_v3.Cluster{
								Http2ProtocolOptions: &envoy_config_core_v3.Http2ProtocolOptions{},
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
			if diff := cmp.Diff(got, want, protocmp.Transform()); diff != "" {
				t.Errorf("loaded yaml:\n  got: %#v\n want: %#v\n diff: %v", got, want, diff)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestLoadAssignmentFromEndpoints(t *testing.T) {
	testData := []struct {
		name      string
		endpoints []*discoveryv1.EndpointSlice
		want      []*envoy_config_endpoint_v3.ClusterLoadAssignment
	}{
		{
			name:      "nil",
			endpoints: nil,
			want:      nil,
		},
		{
			name:      "empty",
			endpoints: []*discoveryv1.EndpointSlice{{}},
			want:      nil,
		},
		{
			name: "ready_and_notready",
			endpoints: []*discoveryv1.EndpointSlice{
				{
					TypeMeta: metav1.TypeMeta{
						APIVersion: discoveryv1.SchemeGroupVersion.String(),
						Kind:       "EndpointSlice",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "foo",
						Name:      "bar-v2drk",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "bar",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:     ptr("port"),
							Port:     ptr(int32(1234)),
							Protocol: ptr(v1.ProtocolTCP),
						},
						{
							Name:     ptr("debug"),
							Port:     ptr(int32(8080)),
							Protocol: ptr(v1.ProtocolTCP),
						},
						{
							Name:     ptr("udp"),
							Port:     ptr(int32(1234)),
							Protocol: ptr(v1.ProtocolUDP),
						},
						{
							Name:     ptr("sctp"),
							Port:     ptr(int32(1234)),
							Protocol: ptr(v1.ProtocolSCTP),
						},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
							NodeName:   ptr("host0"),
						},
						{
							Addresses:  []string{"10.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{Ready: ptr(false)},
							NodeName:   ptr("host0"),
						},
					},
				},

				{
					TypeMeta: metav1.TypeMeta{
						APIVersion: discoveryv1.SchemeGroupVersion.String(),
						Kind:       "EndpointSlice",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "foo",
						Name:      "bar-rs32p",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "bar",
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:     ptr("port"),
							Port:     ptr(int32(1234)),
							Protocol: ptr(v1.ProtocolTCP),
						},
					},
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses:  []string{"10.0.0.3"},
							Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)},
							NodeName:   ptr("host1"),
						},
						{
							Addresses:  []string{"10.0.0.4"},
							Conditions: discoveryv1.EndpointConditions{Ready: ptr(false)},
							NodeName:   ptr("host1"),
						},
					},
				},
			},
			want: []*envoy_config_endpoint_v3.ClusterLoadAssignment{
				{
					ClusterName: "foo:bar:debug",
					Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{
						{
							Locality: &envoy_config_core_v3.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
								lbEndpoint("10.0.0.1", 8080, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 8080, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_DEGRADED),
							},
						},
					},
				},
				{
					ClusterName: "foo:bar:port",
					Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{
						{
							Locality: &envoy_config_core_v3.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
								lbEndpoint("10.0.0.1", 1234, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 1234, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_DEGRADED),
							},
						},
						{
							Locality: &envoy_config_core_v3.Locality{
								Region:  "region0",
								Zone:    "host1",
								SubZone: "host1",
							},
							LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
								lbEndpoint("10.0.0.3", 1234, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.4", 1234, envoy_config_core_v3.SocketAddress_TCP, envoy_config_core_v3.HealthStatus_DEGRADED),
							},
						},
					},
				},
				{
					ClusterName: "foo:bar:udp:udp",
					Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{
						{
							Locality: &envoy_config_core_v3.Locality{
								Region:  "region0",
								Zone:    "host0",
								SubZone: "host0",
							},
							LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
								lbEndpoint("10.0.0.1", 1234, envoy_config_core_v3.SocketAddress_UDP, envoy_config_core_v3.HealthStatus_HEALTHY),
								lbEndpoint("10.0.0.2", 1234, envoy_config_core_v3.SocketAddress_UDP, envoy_config_core_v3.HealthStatus_DEGRADED),
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
			got := cfg.EndpointConfig.LoadAssignmentsFromEndpointSlices(nodes, test.endpoints)
			if diff := cmp.Diff(got, test.want, protocmp.Transform()); diff != "" {
				t.Errorf("endpoints:\n  got: %v\n want: %v\n diff: %v", got, test.want, diff)
			}
		})
	}
}

func TestLocality(t *testing.T) {
	testData := []struct {
		localityConfig *LocalityConfig
		input          string
		want           *envoy_config_core_v3.Locality
	}{
		{
			localityConfig: nil,
			input:          "host0",
			want:           &envoy_config_core_v3.Locality{},
		},
		{
			localityConfig: &LocalityConfig{},
			input:          "host0",
			want:           &envoy_config_core_v3.Locality{},
		},
		{
			localityConfig: &LocalityConfig{
				RegionFrom: &Field{
					Literal: "region",
				},
			},
			input: "host0",
			want: &envoy_config_core_v3.Locality{
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
			want: &envoy_config_core_v3.Locality{
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
			want: &envoy_config_core_v3.Locality{
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
			want: &envoy_config_core_v3.Locality{
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
			want: &envoy_config_core_v3.Locality{
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
		if diff := cmp.Diff(got, test.want, protocmp.Transform()); diff != "" {
			t.Errorf("test %d: locality:\n  %s", i, diff)
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

func TestAllCacheMethods(t *testing.T) {
	xds := cds.NewServer("test", nil)
	cfg := DefaultConfig()
	cs := cfg.ClusterConfig.Store(xds)
	ca, cb := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "a",
		},
		Spec: v1.ServiceSpec{
			ClusterIP: "None",
			Ports: []v1.ServicePort{
				{
					Name: "a",
					Port: 1234,
				},
			},
		},
	}, &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "b",
		},
		Spec: v1.ServiceSpec{
			ClusterIP: "None",
			Ports: []v1.ServicePort{
				{
					Name: "b",
					Port: 4321,
				},
			},
		},
	}
	assertClusters := func(want ...string) {
		t.Helper()
		sort.Strings(want)
		var got []string
		for _, c := range cs.List() {
			got = append(got, c.(interface{ GetName() string }).GetName())
		}
		sort.Strings(got)
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("assertClusters:\n  got: %v\n want: %v\n diff: %v", got, want, diff)
		}
	}
	if err := cs.Add(nil); err == nil {
		t.Fatal("nil add should fail")
	}
	assertClusters()
	if err := cs.Add(&v1.Endpoints{}); err == nil {
		t.Fatal("non-service add should fail")
	}
	assertClusters()
	if err := cs.Add(ca); err != nil {
		t.Fatal(err)
	}
	assertClusters("test:a:a")
	if err := cs.Replace([]interface{}{ca, cb}, "12345"); err != nil {
		t.Fatal(err)
	}
	assertClusters("test:a:a", "test:b:b")
	cb.Spec.Ports[0].Port = 1234
	if err := cs.Update(cb); err != nil {
		t.Fatal(err)
	}
	assertClusters("test:a:a", "test:b:b")
	if err := cs.Delete(cb); err != nil {
		t.Fatal(err)
	}
	assertClusters("test:a:a")
	if err := cs.Delete(ca); err != nil {
		t.Fatal(err)
	}
	assertClusters()

	es := cfg.EndpointConfig.Store(nil, xds)
	ea := &discoveryv1.EndpointSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: discoveryv1.SchemeGroupVersion.String(),
			Kind:       "EndpointSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "a-v2drk",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "a",
			},
		},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr("a"),
			Port: ptr(int32(1234)),
		}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"1.2.3.4"}}},
	}
	eb := &discoveryv1.EndpointSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: discoveryv1.SchemeGroupVersion.String(),
			Kind:       "EndpointSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "b-v2drk",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "b",
			},
		},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr("b"),
			Port: ptr(int32(1234)),
		}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"1.2.3.4"}}},
	}

	assertEndpoints := func(want ...string) {
		t.Helper()
		sort.Strings(want)
		var got []string
		got = append(got, xds.Endpoints.ListKeys()...)
		sort.Strings(got)
		if diff := cmp.Diff(got, want); diff != "" {
			t.Errorf("assertEndpoints:\n  got: %v\n want: %v\n diff: %v", got, want, diff)
		}
	}
	if err := es.Add(nil); err == nil {
		t.Fatal("nil add should error")
	}
	assertEndpoints()
	if err := es.Add(&v1.Service{}); err == nil {
		t.Fatal("non-endpoints add should error")
	}
	assertEndpoints()
	if err := es.Add(ea); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("test:a:a")
	if err := es.Replace([]interface{}{ea, eb}, "837873"); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("test:a:a", "test:b:b")
	eb2 := &discoveryv1.EndpointSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: discoveryv1.SchemeGroupVersion.String(),
			Kind:       "EndpointSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "b-v2drk",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "b",
			},
		},
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr("c"),
			Port: ptr(int32(1234)),
		}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"1.2.3.4"}}},
	}
	if err := es.Update(eb2); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("test:a:a", "test:b:c")
	if err := es.Delete(eb2); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("test:a:a")
	if err := es.Delete(ea); err != nil {
		t.Fatal(err)
	}
	assertEndpoints()
}
