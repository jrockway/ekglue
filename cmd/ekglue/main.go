// Command "ekglue" runs a gRPC server that serves an Envoy CDS and EDS server.
package main

import (
	"context"
	"net/http"

	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/jrockway/ekglue/pkg/k8s"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"k8s.io/client-go/tools/cache"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
)

type kflags struct {
	Kubeconfig string `long:"kubeconfig" env:"KUBECONFIG" description:"kubeconfig to use to connect to the cluster, when running outside of the cluster"`
	Master     string `long:"master" env:"KUBE_MASTER" description:"url of the kubernetes master, only necessary when running outside of the cluster and when it's not specified in the provided kubeconfig"`
}

type flags struct {
	Config        string `short:"c" long:"config" env:"EKGLUE_CONFIG_FILE" description:"config file to read"`
	VersionPrefix string `long:"version_prefix" env:"VERSION_PREFIX" description:"a string to prepend to the version number that we use to identify the generated configuration to envoy and in metrics"`
}

func main() {
	server.AppName = "ekglue"

	f := new(flags)
	server.AddFlagGroup("ekglue", f)
	kf := new(kflags)
	server.AddFlagGroup("Kubernetes", kf)

	drainCh := make(chan struct{})
	server.AddDrainHandler(func() { close(drainCh) })

	server.Setup()

	svc := cds.NewServer(f.VersionPrefix, drainCh)
	server.AddService(func(s *grpc.Server) {
		clusterservice.RegisterClusterDiscoveryServiceServer(s, svc)
		endpointservice.RegisterEndpointDiscoveryServiceServer(s, svc)
		envoy_api_v2.RegisterClusterDiscoveryServiceServer(s, &envoy_api_v2.UnimplementedClusterDiscoveryServiceServer{})
		envoy_api_v2.RegisterEndpointDiscoveryServiceServer(s, &envoy_api_v2.UnimplementedEndpointDiscoveryServiceServer{})
	})
	http.Handle("/clusters", svc.Clusters)
	http.Handle("/endpoints", svc.Endpoints)

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
	if filename := f.Config; filename != "" {
		zap.L().Info("reading config", zap.String("filename", filename))
		var err error
		cfg, err = glue.LoadConfig(filename)
		if err != nil {
			zap.L().Fatal("problem reading config file", zap.String("filename", filename), zap.Error(err))
		}
	} else {
		zap.L().Info("using default config")
	}

	ns := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
	http.Handle("/localities", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		yaml, err := cfg.EndpointConfig.Locality.LocalitiesAsYAML(ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(yaml)
	}))
	zap.L().Info("pre-filling node store")
	if err := watcher.ListNodes(ns); err != nil {
		zap.L().Fatal("problem listing nodes", zap.Error(err))
	}
	go func() {
		if err := watcher.WatchNodes(context.Background(), ns); err != nil {
			zap.L().Fatal("node watch unexpectedly exited", zap.Error(err))
		}
	}()
	go func() {
		if err := watcher.WatchServices(context.Background(), cfg.ClusterConfig.Store(svc)); err != nil {
			zap.L().Fatal("service watch unexpectedly exited", zap.Error(err))
		}
	}()
	go func() {
		if err := watcher.WatchEndpointSlices(context.Background(), cfg.EndpointConfig.Store(ns, svc)); err != nil {
			zap.L().Fatal("endpointslice watch unexpectedly exited", zap.Error(err))
		}
	}()

	server.ListenAndServe()
}
