// Command "cds" runs a gRPC server that serves an Envoy CDS and EDS server.
package main

import (
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

func main() {
	server.AppName = "ekglue-cds"

	svc := xds.NewServer()
	server.AddService(func(s *grpc.Server) {
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, svc)
	})
	server.Setup()

	err := svc.AddClusters([]*envoy_api_v2.Cluster{
		&envoy_api_v2.Cluster{
			Name:           "debug",
			ConnectTimeout: ptypes.DurationProto(time.Second),
		},
	})
	if err != nil {
		zap.L().Fatal("adding test data failed", zap.Error(err))
	}

	server.ListenAndServe()
}
