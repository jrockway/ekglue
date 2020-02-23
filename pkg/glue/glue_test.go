package glue

import (
	"sort"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type"
	"github.com/go-test/deep"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/wrappers"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80),
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
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http", "bar.foo.svc.cluster.local.", 80),
				},
				{
					Name:                 "foo:bar:443",
					ConnectTimeout:       ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STRICT_DNS},
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:443", "bar.foo.svc.cluster.local.", 443),
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
					LoadAssignment:       singleTargetLoadAssignment("foo:bar:http2", "bar.foo.svc.cluster.local.", 80),
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
			if diff := deep.Equal(got, test.want); diff != nil {
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
						Region:      "tests",
						ZoneFrom:    "host",
						SubZoneFrom: "host",
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
			if diff := deep.Equal(got, want); diff != nil {
				t.Errorf("loaded yaml:\n  got: %#v\n want: %#v\n diff: %v", got, want, diff)
			}
		})
	}
}

func TestLoadAssignmentFromEndpoints(t *testing.T) {
	testData := []struct {
		name      string
		endpoints *v1.Endpoints
		want      []*envoy_api_v2.ClusterLoadAssignment
	}{
		{name: "empty"},
	}

	cfg, err := LoadConfig("testdata/load_assignment_from_endpoints_test.yaml")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			cfg.ClusterConfig.ClustersFromService(nil)
			// got := cfg.EndpointConfig.LoadAssignmentFromEndpoints(test.endpoints)
			// if diff := deep.Equal(got, test.want); diff != nil {
			// 	t.Errorf("endpoints:\n  got: %v\n want: %v\n diff: %v", got, test.want, diff)
			// }
		})
	}
}
