package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/jrockway/ekglue/pkg/k8s"
	"github.com/jrockway/ekglue/pkg/xds"
	"k8s.io/klog"
)

var (
	kubeconfig string
	config     = flag.String("config", "", "path to the ekglue config")
	verbose    = flag.Bool("verbose", false, "true to dump cluster YAML with defaults listed")
)

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube", "config"), "path to the kubeconfig for the cluster you want to run again")
	klog.InitFlags(nil)
	flag.Parse()
	w, err := k8s.ConnectOutOfCluster(kubeconfig, "")
	if err != nil {
		klog.Fatalf("connect to k8s cluster: %v", err)
	}

	var cfg *glue.Config
	if *config != "" {
		var err error
		cfg, err = glue.LoadConfig(*config)
		if err != nil {
			klog.Fatalf("load config %q: %v", *config, err)
		}
	} else {
		cfg = glue.DefaultConfig()
	}

	server := xds.NewServer("ekglue-dump-config")
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
