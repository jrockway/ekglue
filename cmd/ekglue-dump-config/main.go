package main

import (
	"flag"
	"fmt"

	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/jrockway/ekglue/pkg/k8s"
	"github.com/jrockway/ekglue/pkg/xds"
	"k8s.io/klog"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "path to the kubeconfig for the cluster you want to run again")
	config     = flag.String("config", "ekglue.yaml", "path to the ekglue config")
	verbose    = flag.Bool("verbose", false, "true to dump cluster YAML with defaults listed")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	w, err := k8s.ConnectOutOfCluster(*kubeconfig, "")
	if err != nil {
		klog.Fatalf("connect to k8s cluster: %v", err)
	}

	cfg, err := glue.LoadConfig(*config)
	if err != nil {
		klog.Fatalf("load config %q: %v", *config, err)
	}

	server := xds.NewServer()
	store := cfg.ClusterConfig.Store(server)
	if err := w.ListServices(store); err != nil {
		klog.Fatalf("list services: %v", err)
	}
	bytes, err := server.ConfigAsYAML(*verbose)
	if err != nil {
		klog.Fatalf("dump yaml: %v", err)
	}
	fmt.Printf("%s\n", bytes)
}
