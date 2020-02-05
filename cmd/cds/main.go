// Command "cds" runs a gRPC server that serves an Envoy CDS and EDS server.
package main

import (
	"context"

	"github.com/jrockway/ekglue/pkg/config"
	"github.com/jrockway/ekglue/pkg/k8s"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

type flags struct {
	Kubeconfig string `long:"kubeconfig" env:"KUBECONFIG" description:"kubeconfig to use to connect to the cluster, when running outside of the cluster"`
}

func main() {
	server.AppName = "ekglue-cds"

	f := new(flags)
	server.AddFlagGroup("ekglue", f)

	svc := xds.NewServer()
	server.AddService(func(s *grpc.Server) {
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, svc)
	})

	server.Setup()

	watcher, err := k8s.ConnectOutOfCluster(f.Kubeconfig)
	if err != nil {
		zap.L().Fatal("problem connecting to cluster via kubeconfig", zap.String("kubeconfig", f.Kubeconfig), zap.Error(err))
	}
	watcher.Config = config.NewConfig()
	watcher.Server = svc
	watcher.Logger = zap.L().Named("ClusterWatcher")
	go watcher.WatchServices(context.Background())

	server.ListenAndServe()
}
