package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/jrockway/opinionated-server/client"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// ClusterWatcher watches services and endpoints inside of a cluster.
type ClusterWatcher struct {
	coreV1Client     rest.Interface
	discoverV1Client rest.Interface

	// For tests, a ListerWatcher that will be used instead of the client-based ListerWatcher.
	testLW cache.ListerWatcher
}

// New returns a ClusterWatcher from a kubernetes config.
func New(config *rest.Config) (*ClusterWatcher, error) {
	config.WrapTransport = client.WrapRoundTripper
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: new client: %w", err)
	}
	return &ClusterWatcher{
		coreV1Client:     clientset.CoreV1().RESTClient(),
		discoverV1Client: clientset.DiscoveryV1().RESTClient(),
	}, nil
}

// ConnectOutOfCluster connects to the API server from outside of the cluster.
func ConnectOutOfCluster(kubeconfig, master string) (*ClusterWatcher, error) {
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build config: %w", err)
	}
	return New(config)
}

// ConnectInCluster connects to the API server from a pod inside the cluster.
func ConnectInCluster() (*ClusterWatcher, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: get in-cluster config: %w", err)
	}
	return New(config)
}

// newListWatch returns a ListerWatcher that watches the configured k8s API object with the built-in
// client.
func (cw *ClusterWatcher) newListWatch(getter cache.Getter, resource, namespace string, fieldSelector fields.Selector) cache.ListerWatcher { //nolint:unparam
	if cw.testLW != nil {
		return cw.testLW
	}
	return cache.NewListWatchFromClient(getter, resource, namespace, fieldSelector)
}

// WatchServices notifes the provided ServiceReceiver of changes to services, in all namespaces.
func (cw *ClusterWatcher) WatchServices(ctx context.Context, s cache.Store) error {
	lw := cw.newListWatch(cw.coreV1Client, "services", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Service{}, s, 0)
	r.Run(ctx.Done())
	return nil
}

// ListServices sends all services to the provided cache.Store.
func (cw *ClusterWatcher) ListServices(s cache.Store) error {
	lw := cw.newListWatch(cw.coreV1Client, "services", "", fields.Everything())
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

// WatchEndpointSlices notifes the provided cache.Store of changes to EndpointSlices, in all namespaces.
func (cw *ClusterWatcher) WatchEndpointSlices(ctx context.Context, s cache.Store) error {
	lw := cw.newListWatch(cw.discoverV1Client, "endpointslices", "", fields.Everything())
	r := cache.NewReflector(lw, &discoveryv1.EndpointSlice{}, s, 0)
	r.Run(ctx.Done())
	return nil
}

// ListEndpointSlices sends all EndpointSlices to the provided cache.Store.
func (cw *ClusterWatcher) ListEndpointSlices(s cache.Store) error {
	lw := cw.newListWatch(cw.discoverV1Client, "endpointslices", "", fields.Everything())
	raw, err := lw.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	list, ok := raw.(*discoveryv1.EndpointSliceList)
	if !ok {
		return fmt.Errorf("unexpected type: %T", raw)
	}
	for _, rawEp := range list.Items {
		ep := rawEp
		if err := s.Add(&ep); err != nil {
			return fmt.Errorf("add endpoint: %v", err)
		}
	}
	return nil
}

// WatchNodes notifes the provided cache.Store of changes to nodes.
func (cw *ClusterWatcher) WatchNodes(ctx context.Context, s cache.Store) error {
	lw := cw.newListWatch(cw.coreV1Client, "nodes", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Node{}, s, time.Minute)
	r.Run(ctx.Done())
	return nil
}

// ListNodes sends all nodes to the provided cache.Store.
func (cw *ClusterWatcher) ListNodes(s cache.Store) error {
	lw := cw.newListWatch(cw.coreV1Client, "nodes", "", fields.Everything())
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
