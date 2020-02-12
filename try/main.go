package main

import (
	"net/http"

	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
)

type flags struct {
	Config string `short:"c" long:"config" env:"EKGLUE_CONFIG_FILE" description:"config file to read"`
}

func main() {
	server.AppName = "ekglue-try"

	f := new(flags)
	server.AddFlagGroup("ekglue", f)
	server.Setup()

	svc := cds.NewServer("")
	server.AddService(func(s *grpc.Server) {
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, svc)
		envoy_api_v2.RegisterEndpointDiscoveryServiceServer(s, svc)
	})
	http.Handle("/clusters", svc.Clusters)
	http.Handle("/endpoints", svc.Endpoints)

	// cfg := glue.DefaultConfig()
	// if filename := f.Config; filename != "" {
	// 	zap.L().Info("reading config", zap.String("filename", filename))
	// 	var err error
	// 	cfg, err = glue.LoadConfig(filename)
	// 	if err != nil {
	// 		zap.L().Fatal("problem reading config file", zap.String("filename", filename), zap.Error(err))
	// 	}
	// }
	me := &envoy_api_v2_endpoint.LbEndpoint{
		HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
			Endpoint: &envoy_api_v2_endpoint.Endpoint{
				Address: &envoy_api_v2_core.Address{
					Address: &envoy_api_v2_core.Address_SocketAddress{
						SocketAddress: &envoy_api_v2_core.SocketAddress{
							Address: "127.0.0.1",
							PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
								PortValue: 8081,
							},
						},
					},
				},
			},
		},
	}
	me2 := &envoy_api_v2_endpoint.LbEndpoint{
		HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
			Endpoint: &envoy_api_v2_endpoint.Endpoint{
				Address: &envoy_api_v2_core.Address{
					Address: &envoy_api_v2_core.Address_SocketAddress{
						SocketAddress: &envoy_api_v2_core.SocketAddress{
							Address: "127.0.0.2",
							PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
								PortValue: 8081,
							},
						},
					},
				},
			},
		},
	}
	err := svc.Endpoints.Add([]xds.Resource{
		&envoy_api_v2.ClusterLoadAssignment{
			ClusterName: "foo",
			Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{
				{
					Locality: &envoy_api_v2_core.Locality{
						Region:  "earth",
						Zone:    "localdomain",
						SubZone: "localhost",
					},
					LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{me},
				},
				{
					Locality: &envoy_api_v2_core.Locality{
						Region:  "earth",
						Zone:    "afardomain",
						SubZone: "afarhost",
					},
					LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{me2},
				},
			},
		}})
	if err != nil {
		zap.L().Fatal("error adding endpoints", zap.Error(err))
	}

	server.ListenAndServe()
}
