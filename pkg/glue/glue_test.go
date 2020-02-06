package glue

import (
	"sort"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/go-test/deep"
	"github.com/golang/protobuf/ptypes"
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
		}, {
			name: "named port",
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
	}

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			got := cfg.ClusterConfig.ClustersFromService(test.service)
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			sort.Slice(test.want, func(i, j int) bool { return test.want[i].Name < test.want[j].Name })
			if diff := deep.Equal(got, test.want); diff != nil {
				t.Errorf("clusters: \n  got: %v\n want: %v\n diff: %v", got, test.want, diff)
			}
		})
	}
}
