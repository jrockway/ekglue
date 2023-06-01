package k8s

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// These tests mostly exist to check that the code actually runs, but fail to test things like "does
// NewListWatch('services') return v1.Service objects".  This situation could be improved.

func TestConnect(t *testing.T) {
	if _, err := New(&rest.Config{}); err != nil {
		t.Errorf("problem creating empty config: %v", err)
	}
	if _, err := ConnectOutOfCluster("", ""); err == nil {
		t.Error("expected error when connecting without kubeconfig or master")
	}
	os.Setenv("KUBERNETES_SERVICE_HOST", "")
	os.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := ConnectInCluster(); err == nil {
		t.Error("expected error when connecting outside of a cluster")
	}
}

func TestListers(t *testing.T) {
	store := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)

	var list runtime.Object
	cw := &ClusterWatcher{
		testLW: &cache.ListWatch{
			ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
				return list, nil
			},
			WatchFunc: func(metav1.ListOptions) (watch.Interface, error) {
				return nil, errors.New("watch failed")
			},
		},
	}
	list = &discoveryv1.EndpointSliceList{
		Items: []discoveryv1.EndpointSlice{{}},
	}
	if err := cw.ListEndpointSlices(store); err != nil {
		t.Errorf("ListEndpointSlices: %v", err)
	}
	list = &v1.ServiceList{
		Items: []v1.Service{{}},
	}
	if err := cw.ListServices(store); err != nil {
		t.Errorf("ListServices: %v", err)
	}
	list = &v1.NodeList{
		Items: []v1.Node{{}},
	}
	if err := cw.ListNodes(store); err != nil {
		t.Errorf("ListNodes: %v", err)
	}
}

type watcher struct {
	eventCh chan watch.Event
	doneCh  chan struct{}
}

func (w *watcher) Stop() {
	close(w.eventCh)
}

func (w *watcher) ResultChan() <-chan watch.Event {
	return w.eventCh
}

func newWatcher(objects []runtime.Object) *watcher {
	w := &watcher{
		eventCh: make(chan watch.Event),
		doneCh:  make(chan struct{}),
	}
	go func() {
		for _, obj := range objects {
			w.eventCh <- watch.Event{
				Type:   watch.Added,
				Object: obj,
			}
			w.eventCh <- watch.Event{
				Type:   watch.Modified,
				Object: obj,
			}
		}
		close(w.doneCh)
	}()
	return w
}

func TestWatchers(t *testing.T) {
	testData := []struct {
		run  func(context.Context, *ClusterWatcher, cache.Store)
		list runtime.Object
		add  []runtime.Object
		want []string
	}{
		{
			run: func(ctx context.Context, cw *ClusterWatcher, s cache.Store) {
				cw.WatchNodes(ctx, s)
			},
			list: &v1.NodeList{},
			add: []runtime.Object{
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "hostname",
					},
				},
			},
			want: []string{"hostname"},
		},
		{
			run: func(ctx context.Context, cw *ClusterWatcher, s cache.Store) {
				cw.WatchServices(ctx, s)
			},
			list: &v1.NodeList{},
			add: []runtime.Object{
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "service",
					},
				},
			},
			want: []string{"default/service"},
		},
		{
			run: func(ctx context.Context, cw *ClusterWatcher, s cache.Store) {
				cw.WatchEndpointSlices(ctx, s)
			},
			list: &v1.NodeList{},
			add: []runtime.Object{
				&discoveryv1.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "service-ff00abcd",
					},
				},
			},
			want: []string{"default/service-ff00abcd"},
		},
	}
	for i, test := range testData {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		s := cache.NewStore(cache.MetaNamespaceKeyFunc)
		w := newWatcher(test.add)
		cw := &ClusterWatcher{
			testLW: &cache.ListWatch{
				ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
					return test.list, nil
				},
				WatchFunc: func(metav1.ListOptions) (watch.Interface, error) {
					return w, nil
				},
			},
		}
		go test.run(ctx, cw, s)
		select {
		case <-ctx.Done():
			t.Fatalf("test %d: timeout", i)
		case <-w.doneCh:
		}
		c()
		got := s.ListKeys()
		want := test.want
		sort.Strings(got)
		sort.Strings(want)
		if diff := cmp.Diff(got, test.want); diff != "" {
			t.Errorf("test %d: got != want:\n diff: %s", i, diff)
		}
	}
}
