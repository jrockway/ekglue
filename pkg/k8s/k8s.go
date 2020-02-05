package k8s

import (
	"context"
	"errors"
	"fmt"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/jrockway/ekglue/pkg/config"
	"github.com/jrockway/ekglue/pkg/xds"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// ClusterWatcher watches services and endpoints inside of a cluster.
type ClusterWatcher struct {
	coreV1Client rest.Interface
	Logger       *zap.Logger
	Config       *config.Config
	Server       *xds.Server
}

// ConnectOutOfCluster connects to the API server from outside of the cluster.
func ConnectOutOfCluster(kubeconfig string) (*ClusterWatcher, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: new client: %w", err)
	}
	return &ClusterWatcher{coreV1Client: clientset.CoreV1().RESTClient()}, nil
}

// These methods implement cache.Store for v1.Service, mapping them to clusters and actually storing
// them in the xDS server.

func (cw *ClusterWatcher) Add(obj interface{}) error {
	cw.Logger.Debug("add", zap.Any("obj", obj))
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("add service: got non-service object %#v", obj)
	}
	if err := cw.Server.AddClusters(cw.Config.ClusterConfig.ClustersFromService(svc)); err != nil {
		return fmt.Errorf("add service: clusters: %w", err)
	}
	return nil
}
func (cw *ClusterWatcher) Update(obj interface{}) error {
	cw.Logger.Debug("update", zap.Any("obj", obj))
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("update service: got non-service object %#v", obj)
	}
	if err := cw.Server.AddClusters(cw.Config.ClusterConfig.ClustersFromService(svc)); err != nil {
		return fmt.Errorf("update service: add clusters: %w", err)
	}
	return nil
}
func (cw *ClusterWatcher) Delete(obj interface{}) error {
	cw.Logger.Debug("delete", zap.Any("obj", obj))
	svc, ok := obj.(*v1.Service)
	if !ok {
		return fmt.Errorf("delete service: got non-service object %#v", obj)
	}
	cs := cw.Config.ClusterConfig.ClustersFromService(svc)
	for _, c := range cs {
		cw.Server.DeleteCluster(c.GetName())
	}
	return nil
}

func (cw *ClusterWatcher) List() []interface{} {
	cw.Logger.Debug("List")
	cs := cw.Server.ListClusters()
	var result []interface{}
	for _, c := range cs {
		result = append(result, c)
	}
	return result
}

func (cw *ClusterWatcher) ListKeys() []string {
	cw.Logger.Debug("ListKeys")
	cs := cw.Server.ListClusters()
	var result []string
	for _, c := range cs {
		result = append(result, c.GetName())
	}
	return result
}
func (cw *ClusterWatcher) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("clusterwatcher.Get unimplemented")
}
func (cw *ClusterWatcher) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, errors.New("clusterwatcher.GetByKey unimplemented")
}
func (cw *ClusterWatcher) Replace(objs []interface{}, _ string) error {
	cw.Logger.Debug("replace", zap.Any("objs", objs))
	var clusters []*envoy_api_v2.Cluster
	for _, obj := range objs {
		svc, ok := obj.(*v1.Service)
		if !ok {
			return fmt.Errorf("replace services: got non-service object %#v", obj)
		}
		cs := cw.Config.ClusterConfig.ClustersFromService(svc)
		clusters = append(clusters, cs...)
	}
	if err := cw.Server.ReplaceClusters(clusters); err != nil {
		return fmt.Errorf("replace services: replace clusters: %w", err)
	}
	return nil
}
func (cw *ClusterWatcher) Resync() error {
	return errors.New("clusterwatcher.Resync unimplemented")
}

// WatchServices notifes the provided ServiceReceiver of changes to services, in all namespaces.
func (cw *ClusterWatcher) WatchServices(ctx context.Context) error {
	lw := cache.NewListWatchFromClient(cw.coreV1Client, "services", "", fields.Everything())
	r := cache.NewReflector(lw, &v1.Service{}, cw, 0)
	r.Run(ctx.Done())
	return nil
}
