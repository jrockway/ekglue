// Command "cds" runs a gRPC server that serves an Envoy CDS and EDS server.
package main

import (
	"context"

	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/jrockway/ekglue/pkg/k8s"
	"github.com/jrockway/ekglue/pkg/xds"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
)

type kflags struct {
	Kubeconfig string `long:"kubeconfig" env:"KUBECONFIG" description:"kubeconfig to use to connect to the cluster, when running outside of the cluster"`
	Master     string `long:"master" env:"KUBE_MASTER" description:"url of the kubernetes master, only necessary when running outside of the cluster and when it's not specified in the provided kubeconfig"`
}

func main() {
	server.AppName = "ekglue-cds"

	kf := new(kflags)
	server.AddFlagGroup("Kubernetes", kf)

	svc := xds.NewServer()
	server.AddService(func(s *grpc.Server) {
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, svc)
	})

	server.Setup()

	var watcher *k8s.ClusterWatcher
	if kf.Kubeconfig != "" || kf.Master != "" {
		var err error
		zap.L().Info("connecting to kubernetes, outside of cluster")
		watcher, err = k8s.ConnectOutOfCluster(kf.Kubeconfig, kf.Master)
		if err != nil {
			zap.L().Fatal("problem connecting to cluster via kubeconfig", zap.String("kubeconfig", kf.Kubeconfig), zap.String("master", kf.Master), zap.Error(err))
		}
	} else {
		var err error
		zap.L().Info("connecting to kubernetes, running in-cluster")
		watcher, err = k8s.ConnectInCluster()
		if err != nil {
			zap.L().Fatal("problem connecting to cluster", zap.Error(err))
		}
	}

	cfg := glue.DefaultConfig()
	go watcher.WatchServices(context.Background(), cfg.ClusterConfig.Store(svc))

	server.ListenAndServe()
}
