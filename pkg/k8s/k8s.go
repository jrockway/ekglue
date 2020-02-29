package k8s

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jrockway/opinionated-server/client"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// ClusterWatcher watches services and endpoints inside of a cluster.
type ClusterWatcher struct {
	coreV1Client rest.Interface
}

// ConnectOutOfCluster connects to the API server from outside of the cluster.
func ConnectOutOfCluster(kubeconfig string, master string) (*ClusterWatcher, error) {
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build config: %w", err)
	}
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return client.WrapRoundTripper(rt)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: new client: %w", err)
	}
	return &ClusterWatcher{coreV1Client: clientset.CoreV1().RESTClient()}, nil
}

// ConnectInCluster connects to the API server from a pod inside the cluster.
func ConnectInCluster() (*ClusterWatcher, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: get in-cluster config: %w", err)
	}
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return client.WrapRoundTripper(rt)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: new client: %w", err)
	}
	return &ClusterWatcher{coreV1Client: clientset.CoreV1().RESTClient()}, nil
}

// WatchServices notifes the provided ServiceReceiver of changes to services, in all namespaces.
func (cw *ClusterWatcher) WatchServices(ctx context.Context, s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "services", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Service{}, s, 0)
	r.Run(ctx.Done())
	return nil
}

// ListServices sends all services to the provided cache.Store.
func (cw *ClusterWatcher) ListServices(s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "services", "", fields.Everything())
	raw, err := lw.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	for _, rawSvc := range raw.(*v1.ServiceList).Items {
		svc := rawSvc
		if err := s.Add(&svc); err != nil {
			return fmt.Errorf("add service: %v", err)
		}
	}
	return nil
}

// WatchEndpoints notifes the provided EndpointReceiver of changes to endpoints, in all namespaces.
func (cw *ClusterWatcher) WatchEndpoints(ctx context.Context, s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "endpoints", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Endpoints{}, s, 0)
	r.Run(ctx.Done())
	return nil
}

// ListEndpoints sends all endpoints to the provided cache.Store.
func (cw *ClusterWatcher) ListEndpoints(s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "endpoints", "", fields.Everything())
	raw, err := lw.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	for _, rawEp := range raw.(*v1.EndpointsList).Items {
		ep := rawEp
		if err := s.Add(&ep); err != nil {
			return fmt.Errorf("add endpoint: %v", err)
		}
	}
	return nil
}

// WatchNodes notifes the provided cache.Store of changes to nodes.
func (cw *ClusterWatcher) WatchNodes(ctx context.Context, s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "nodes", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Node{}, s, time.Minute)
	r.Run(ctx.Done())
	return nil
}

// ListNodes sends all nodes to the provided cache.Store.
func (cw *ClusterWatcher) ListNodes(s cache.Store) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "nodes", "", fields.Everything())
	raw, err := lw.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	for _, rawNode := range raw.(*v1.NodeList).Items {
		node := rawNode // this is because &rawNode points at the final node after the loop exits
		if err := s.Add(&node); err != nil {
			return fmt.Errorf("add node: %v", err)
		}
	}
	return nil
}
