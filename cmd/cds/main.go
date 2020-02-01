// Command "cds" runs a gRPC server that serves an Envoy CDS and EDS server.
package main

import (
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

func main() {
	server.AppName = "ekglue-cds"

	svc := new(xds.Server)

	server.AddService(func(s *grpc.Server) {
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, svc)
	})

	server.Setup()
	server.ListenAndServe()
}
