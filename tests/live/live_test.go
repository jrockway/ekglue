package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2" // for tests
	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapio"
	"go.uber.org/zap/zaptest"
	"golang.org/x/exp/constraints"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func get(t *testing.T, url string) error {
	t.Helper()
	var ok bool
	for i := 0; i < 20; i++ {
		req, err := http.NewRequest("GET", url, http.NoBody)
		if err != nil {
			return fmt.Errorf("prepare request: %v", err)
		}
		ctx, c := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req = req.WithContext(ctx)
		res, err := http.DefaultClient.Do(req)
		c()
		if err != nil {
			t.Logf("get %v: attempt %d: %v", url, i, err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		res.Body.Close()
		if got, want := res.StatusCode, http.StatusOK; got != want {
			t.Logf("get %v: attempt %d: status code %d", url, i, got)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.Logf("get %v: attempt %d: ok", url, i)
		ok = true
		break
	}
	if !ok {
		return errors.New("failed after 20 attempts")
	}
	return nil
}

var dynamicConfig = &glue.Config{
	ClusterConfig: &glue.ClusterConfig{
		BaseConfig: &envoy_config_cluster_v3.Cluster{
			ConnectTimeout: durationpb.New(time.Second),
			ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{
				Type: envoy_config_cluster_v3.Cluster_EDS,
			},
			EdsClusterConfig: &envoy_config_cluster_v3.Cluster_EdsClusterConfig{
				EdsConfig: &envoy_config_core_v3.ConfigSource{
					ConfigSourceSpecifier: &envoy_config_core_v3.ConfigSource_ApiConfigSource{
						ApiConfigSource: &envoy_config_core_v3.ApiConfigSource{
							ApiType:             envoy_config_core_v3.ApiConfigSource_GRPC,
							TransportApiVersion: envoy_config_core_v3.ApiVersion_V3,
							GrpcServices: []*envoy_config_core_v3.GrpcService{{
								TargetSpecifier: &envoy_config_core_v3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &envoy_config_core_v3.GrpcService_EnvoyGrpc{
									ClusterName: "xds",
								}},
							}},
						},
					},
					InitialFetchTimeout: durationpb.New(time.Second),
					ResourceApiVersion:  envoy_config_core_v3.ApiVersion_V3,
				},
			},
		},
	},
	EndpointConfig: &glue.EndpointConfig{},
}

// This config tests that we ignore V2 requests, and that Envoy rejects our replies that mention V2.
var dynamicV2config = &glue.Config{
	ClusterConfig: &glue.ClusterConfig{
		BaseConfig: &envoy_config_cluster_v3.Cluster{
			ConnectTimeout: durationpb.New(time.Second),
			ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{
				Type: envoy_config_cluster_v3.Cluster_EDS,
			},
			EdsClusterConfig: &envoy_config_cluster_v3.Cluster_EdsClusterConfig{
				EdsConfig: &envoy_config_core_v3.ConfigSource{
					ConfigSourceSpecifier: &envoy_config_core_v3.ConfigSource_ApiConfigSource{
						ApiConfigSource: &envoy_config_core_v3.ApiConfigSource{
							ApiType:             envoy_config_core_v3.ApiConfigSource_GRPC,
							TransportApiVersion: envoy_config_core_v3.ApiVersion_V2, //nolint:staticcheck
							GrpcServices: []*envoy_config_core_v3.GrpcService{{
								TargetSpecifier: &envoy_config_core_v3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &envoy_config_core_v3.GrpcService_EnvoyGrpc{
									ClusterName: "xds",
								}},
							}},
						},
					},
					InitialFetchTimeout: durationpb.New(time.Second),
					ResourceApiVersion:  envoy_config_core_v3.ApiVersion_V2, //nolint:staticcheck
				},
			},
		},
	},
	EndpointConfig: &glue.EndpointConfig{},
}

func boolp(x bool) *bool {
	return &x
}

func stringp(x string) *string {
	return &x
}

func int32p[T constraints.Integer](x T) *int32 {
	y := int32(x)
	return &y
}

func protocolp(x v1.Protocol) *v1.Protocol {
	return &x
}

func TestXDS(t *testing.T) {
	testData := []struct {
		name, configFile string
		config           *glue.Config
		push             func(addr *net.TCPAddr, endpointStore cache.Store, clusterStore cache.Store)
		wantFail         bool
	}{
		{
			name:       "cds cluster with dns endpoint",
			configFile: "envoy-cds.yaml",
			config: &glue.Config{
				ClusterConfig: &glue.ClusterConfig{
					BaseConfig: &envoy_config_cluster_v3.Cluster{
						ConnectTimeout: durationpb.New(time.Second),
						DnsResolvers: []*envoy_config_core_v3.Address{{
							Address: &envoy_config_core_v3.Address_SocketAddress{
								SocketAddress: &envoy_config_core_v3.SocketAddress{
									Address: "127.0.0.1",
									PortSpecifier: &envoy_config_core_v3.SocketAddress_PortValue{
										PortValue: 5353,
									},
								},
							},
						}},
						DnsLookupFamily:     envoy_config_cluster_v3.Cluster_V4_ONLY,
						DnsRefreshRate:      durationpb.New(100 * time.Millisecond),
						UseTcpForDnsLookups: true,
					},
				},
			},
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				cs.Add(&v1.Service{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Spec: v1.ServiceSpec{
						ClusterIP: "None",
						Ports: []v1.ServicePort{{
							Name: "http",
							Port: int32(addr.Port),
						}},
					},
				})
			},
		},
		{
			name:       "eds endpoints from static cluster",
			configFile: "envoy-eds-only.yaml",
			config:     glue.DefaultConfig(),
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				es.Add(&discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web-abc12",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "web",
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"127.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(true),
								Serving:     boolp(true),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-afb4e",
								UID:       "0e9408db-1e6d-4231-8fcb-b558f0e5a548",
							},
						},
						{
							Addresses: []string{"127.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(false),
								Serving:     boolp(false),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-nrws4",
								UID:       "39416709-f96f-42be-ac00-b5657a889e95",
							},
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:        stringp("http"),
							Port:        int32p(addr.Port),
							AppProtocol: stringp("http"),
							Protocol:    protocolp(v1.ProtocolTCP),
						},
					},
				})
			},
		},
		{
			name:       "cds with eds endpoints",
			configFile: "envoy-cds.yaml",
			config:     dynamicConfig,
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				cs.Add(&v1.Service{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Spec: v1.ServiceSpec{
						ClusterIP: "None",
						Ports: []v1.ServicePort{{
							Name: "http",
							Port: int32(addr.Port),
						}},
					},
				})
				es.Add(&discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web-abc12",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "web",
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"127.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(true),
								Serving:     boolp(true),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-afb4e",
								UID:       "0e9408db-1e6d-4231-8fcb-b558f0e5a548",
							},
						},
						{
							Addresses: []string{"127.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(false),
								Serving:     boolp(false),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-nrws4",
								UID:       "39416709-f96f-42be-ac00-b5657a889e95",
							},
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:        stringp("http"),
							Port:        int32p(addr.Port),
							AppProtocol: stringp("http"),
							Protocol:    protocolp(v1.ProtocolTCP),
						},
					},
				})
			},
		}, {
			name:       "cds with eds endpoints (reverse order)",
			configFile: "envoy-cds.yaml",
			config:     dynamicConfig,
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				es.Add(&discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web-abc12",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "web",
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"127.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(true),
								Serving:     boolp(true),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-afb4e",
								UID:       "0e9408db-1e6d-4231-8fcb-b558f0e5a548",
							},
						},
						{
							Addresses: []string{"127.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(false),
								Serving:     boolp(false),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-nrws4",
								UID:       "39416709-f96f-42be-ac00-b5657a889e95",
							},
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:        stringp("http"),
							Port:        int32p(addr.Port),
							AppProtocol: stringp("http"),
							Protocol:    protocolp(v1.ProtocolTCP),
						},
					},
				})
				cs.Add(&v1.Service{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Spec: v1.ServiceSpec{
						ClusterIP: "None",
						Ports: []v1.ServicePort{{
							Name: "http",
							Port: int32(addr.Port),
						}},
					},
				})
			},
		}, {
			name:       "cds with v2 eds endpoints (should break)",
			configFile: "envoy-cds.yaml",
			config:     dynamicV2config,
			wantFail:   true,
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				es.Add(&discoveryv1.EndpointSlice{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web-abc12",
						Labels: map[string]string{
							discoveryv1.LabelServiceName: "web",
						},
					},
					AddressType: discoveryv1.AddressTypeIPv4,
					Endpoints: []discoveryv1.Endpoint{
						{
							Addresses: []string{"127.0.0.1"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(true),
								Serving:     boolp(true),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-afb4e",
								UID:       "0e9408db-1e6d-4231-8fcb-b558f0e5a548",
							},
						},
						{
							Addresses: []string{"127.0.0.2"},
							Conditions: discoveryv1.EndpointConditions{
								Ready:       boolp(false),
								Serving:     boolp(false),
								Terminating: boolp(false),
							},
							TargetRef: &v1.ObjectReference{
								Kind:      "Pod",
								Namespace: "test",
								Name:      "web-ccd69c978-nrws4",
								UID:       "39416709-f96f-42be-ac00-b5657a889e95",
							},
						},
					},
					Ports: []discoveryv1.EndpointPort{
						{
							Name:        stringp("http"),
							Port:        int32p(addr.Port),
							AppProtocol: stringp("http"),
							Protocol:    protocolp(v1.ProtocolTCP),
						},
					},
				})
				cs.Add(&v1.Service{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Spec: v1.ServiceSpec{
						ClusterIP: "None",
						Ports: []v1.ServicePort{{
							Name: "http",
							Port: int32(addr.Port),
						}},
					},
				})
			},
		},
	}
	// find the Envoy binary.
	envoy := os.Getenv("ENVOY_PATH")
	if envoy == "" {
		var err error
		envoy, err = exec.LookPath("envoy")
		if err != nil {
			t.Logf("look for envoy in $PATH: %v", err)
			envoy = ""
		}
	}
	if envoy == "" {
		t.Skip("envoy binary not found; set ENVOY_PATH")
	}
	envoyLogLevel := os.Getenv("ENVOY_LOG_LEVEL")
	if envoyLogLevel == "" {
		envoyLogLevel = "critical"
	}

	// Serve DNS.
	dns.HandleFunc("svc.cluster.local.", func(w dns.ResponseWriter, req *dns.Msg) {
		res := new(dns.Msg)
		res.SetReply(req)
		dom := "xxx"
		if len(req.Question) == 1 {
			dom = req.Question[0].Name
		}
		zap.L().Named("dns").Info("query", zap.String("domain", dom))
		res.Answer = append(res.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: dom, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1},
			A:   net.IPv4(127, 0, 0, 1).To4(),
		})
		w.WriteMsg(res)
	})
	d := &dns.Server{
		Addr: "127.0.0.1:5353",
		Net:  "tcp",
	}
	go d.ListenAndServe()
	defer d.Shutdown()

	for _, test := range testData {
		t.Run(test.name, func(t *testing.T) {
			// Setup per-test logging.
			logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
			defer logger.Sync()
			restoreLogger := zap.ReplaceGlobals(logger)
			defer restoreLogger()

			// Serve a small HTTP page we can retrieve through Envoy.
			hl, err := net.Listen("tcp", "")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			gotReqCh := make(chan struct{})

			hs := &http.Server{}
			hs.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("Hello, world!"))
				go func() { gotReqCh <- struct{}{} }()
			})
			go hs.Serve(hl)
			defer func() {
				hs.Close()
				hl.Close()
			}()

			// Serve the xDS RPC service, to allow Envoy to discover the server above.
			gl, err := net.Listen("tcp", "127.0.0.1:9090")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			gs := grpc.NewServer(grpc.StreamInterceptor(loggingStreamServerInterceptor(logger.Named("grpc"))))
			server := cds.NewServer("test-", nil)
			clusterservice.RegisterClusterDiscoveryServiceServer(gs, server)
			endpointservice.RegisterEndpointDiscoveryServiceServer(gs, server)
			envoy_api_v2.RegisterClusterDiscoveryServiceServer(gs, &envoy_api_v2.UnimplementedClusterDiscoveryServiceServer{})
			envoy_api_v2.RegisterEndpointDiscoveryServiceServer(gs, &envoy_api_v2.UnimplementedEndpointDiscoveryServiceServer{})
			go gs.Serve(gl)
			defer func() {
				gs.Stop()
				gl.Close()
			}()

			// Start Envoy.
			cmd := exec.Command(envoy, "-c", test.configFile, "-l", envoyLogLevel)
			redirectToLog(logger.Named("envoy"), cmd)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start envoy: %v", err)
			}
			defer func() {
				cmd.Process.Kill()
				cmd.Wait()
			}()

			// Wait for Envoy to start up.
			if err := get(t, "http://localhost:9091/ping"); err != nil {
				t.Fatalf("envoy never started: %v", err)
			}

			// Push xds information.
			nodes := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
			test.push(hl.Addr().(*net.TCPAddr), test.config.EndpointConfig.Store(nodes, server), test.config.ClusterConfig.Store(server))

			if !test.wantFail {
				// Try getting a request through the proxy.
				if err := get(t, "http://localhost:9091/proxy/hello"); err != nil {
					t.Fatalf("proxied request never succeeded: %v", err)
				}

				// See that this actually called into our handler and isn't just some random other server
				// that returned 200 OK.
				select {
				case <-time.After(100 * time.Millisecond):
					t.Fatal("timeout waiting for ping from http handler")
				case <-gotReqCh:
				}
			} else if err := get(t, "http://localhost:9091/proxy/hello"); err == nil {
				t.Fatal("proxied request unexpectedly succeeded")
			}
		})
	}
}

func redirectToLog(l *zap.Logger, cmd *exec.Cmd) {
	cmd.Stderr = &zapio.Writer{
		Log:   l.Named("stderr"),
		Level: zapcore.InfoLevel,
	}
	cmd.Stdout = &zapio.Writer{
		Log:   l.Named("stdout"),
		Level: zapcore.InfoLevel,
	}
}

type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *wrappedServerStream) Context() context.Context {
	return s.ctx
}

func loggingStreamServerInterceptor(l *zap.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ctxzap.ToContext(stream.Context(), l)
		wrapped := &wrappedServerStream{ServerStream: stream, ctx: ctx}
		return handler(srv, wrapped)
	}
}
