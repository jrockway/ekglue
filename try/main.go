package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
)

type flags struct {
	Config string `short:"c" long:"config" env:"EKGLUE_CONFIG_FILE" description:"config file to read"`
}

func ep(host string, port uint32) *envoy_api_v2_endpoint.LbEndpoint {
	return &envoy_api_v2_endpoint.LbEndpoint{
		HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
			Endpoint: &envoy_api_v2_endpoint.Endpoint{
				Address: &envoy_api_v2_core.Address{
					Address: &envoy_api_v2_core.Address_SocketAddress{
						SocketAddress: &envoy_api_v2_core.SocketAddress{
							Address: host,
							PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
								PortValue: port,
							},
						},
					},
				},
			},
		},
	}
}

func main() {
	server.AppName = "ekglue-try"

	f := new(flags)
	server.AddFlagGroup("ekglue", f)
	server.Setup()

	svc := cds.NewServer("", nil)
	server.AddService(func(s *grpc.Server) {
		clusterservice.RegisterClusterDiscoveryServiceServer(s, svc)
		endpointservice.RegisterEndpointDiscoveryServiceServer(s, svc)
	})
	http.Handle("/clusters", svc.Clusters)
	http.Handle("/endpoints", svc.Endpoints)

	fast := http.Server{
		Addr: "0.0.0.0:1234",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("fast"))
		}),
	}
	slow := http.Server{
		Addr: "0.0.0.0:1235",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			time.Sleep(time.Second)
			w.WriteHeader(http.StatusGatewayTimeout)
			w.Write([]byte("slow"))
		}),
	}

	err := svc.Endpoints.Add(context.Background(), []xds.Resource{
		&envoy_api_v2.ClusterLoadAssignment{
			ClusterName: "foo",
			Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{
				{
					Locality: &envoy_api_v2_core.Locality{
						Region:  "earth",
						Zone:    "localdomain",
						SubZone: "localhost",
					},
					LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
						ep("127.0.0.1", 1234),
						ep("127.0.0.2", 1234),
					},
				},
				{
					Locality: &envoy_api_v2_core.Locality{
						Region:  "earth",
						Zone:    "afardomain",
						SubZone: "afarhost",
					},
					LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{
						ep("127.0.0.3", 1235),
						ep("127.0.0.4", 1235),
					},
				},
			},
		}})
	if err != nil {
		zap.L().Fatal("error adding endpoints", zap.Error(err))
	}

	go fast.ListenAndServe()
	go slow.ListenAndServe()
	server.ListenAndServe()
}
