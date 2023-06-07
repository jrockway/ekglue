package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/jrockway/ekglue/pkg/k8s"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/cache"
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
		klog.Infof("using default config")
	}

	server := cds.NewServer("ekglue-dump-config", nil)

	nodes := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := w.ListNodes(nodes); err != nil {
		klog.Fatalf("list nodes: %v", err)
	}
	if err := w.ListEndpointSlices(cfg.EndpointConfig.Store(nodes, server)); err != nil {
		klog.Fatalf("list endpointslices: %v", err)
	}
	if err := w.ListServices(cfg.ClusterConfig.Store(server)); err != nil {
		klog.Fatalf("list services: %v", err)
	}
	lBytes, err := cfg.EndpointConfig.Locality.LocalitiesAsYAML(nodes)
	if err != nil {
		klog.Fatalf("dump locality yaml: %v", err)
	}
	epBytes, err := server.Endpoints.ConfigAsYAML(*verbose)
	if err != nil {
		klog.Fatalf("dump endpoints yaml: %v", err)
	}
	cBytes, err := server.Clusters.ConfigAsYAML(*verbose)
	if err != nil {
		klog.Fatalf("dump clusters yaml: %v", err)
	}

	fmt.Printf("%s\n---\n", bytes.TrimSpace(lBytes))
	fmt.Printf("%s\n---\n", bytes.TrimSpace(epBytes))
	fmt.Printf("%s\n", bytes.TrimSpace(cBytes))
}
