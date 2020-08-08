package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/golang/protobuf/ptypes"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/glue"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func get(t *testing.T, url string) error {
	t.Helper()
	var ok bool
	for i := 0; i < 20; i++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("prepare request: %v", err)
		}
		ctx, c := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer c()
		req = req.WithContext(ctx)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("ping envoy: attempt %d: %v", i, err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.Logf("ping envoy: attempt %d: ok", i)
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
		BaseConfig: &envoy_api_v2.Cluster{
			ConnectTimeout: ptypes.DurationProto(time.Second),
			ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{
				Type: envoy_api_v2.Cluster_EDS,
			},
			EdsClusterConfig: &envoy_api_v2.Cluster_EdsClusterConfig{
				EdsConfig: &envoy_api_v2_core.ConfigSource{
					ConfigSourceSpecifier: &envoy_api_v2_core.ConfigSource_ApiConfigSource{
						ApiConfigSource: &envoy_api_v2_core.ApiConfigSource{
							ApiType:             envoy_api_v2_core.ApiConfigSource_GRPC,
							TransportApiVersion: envoy_api_v2_core.ApiVersion_V3,
							GrpcServices: []*envoy_api_v2_core.GrpcService{{
								TargetSpecifier: &envoy_api_v2_core.GrpcService_EnvoyGrpc_{EnvoyGrpc: &envoy_api_v2_core.GrpcService_EnvoyGrpc{
									ClusterName: "xds",
								}},
							}},
						},
					},
					InitialFetchTimeout: ptypes.DurationProto(time.Second),
					ResourceApiVersion:  envoy_api_v2_core.ApiVersion_V2,
				},
			},
		},
	},
	EndpointConfig: &glue.EndpointConfig{},
}

func TestXDS(t *testing.T) {
	testData := []struct {
		name, configFile string
		config           *glue.Config
		push             func(addr *net.TCPAddr, endpointStore cache.Store, clusterStore cache.Store)
	}{
		{
			name:       "cds cluster with dns endpoint",
			configFile: "envoy-cds.yaml",
			config: &glue.Config{
				ClusterConfig: &glue.ClusterConfig{
					BaseConfig: &envoy_api_v2.Cluster{
						ConnectTimeout: ptypes.DurationProto(time.Second),
						DnsResolvers: []*envoy_api_v2_core.Address{{
							Address: &envoy_api_v2_core.Address_SocketAddress{
								SocketAddress: &envoy_api_v2_core.SocketAddress{
									Address: "127.0.0.1",
									PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
										PortValue: 5353,
									},
								},
							},
						}},
						DnsLookupFamily:     envoy_api_v2.Cluster_V4_ONLY,
						DnsRefreshRate:      ptypes.DurationProto(100 * time.Millisecond),
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
				es.Add(&v1.Endpoints{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Subsets: []v1.EndpointSubset{{
						Addresses: []v1.EndpointAddress{{
							IP: "127.0.0.1",
						}},
						NotReadyAddresses: []v1.EndpointAddress{{
							IP: "127.0.0.2",
						}},
						Ports: []v1.EndpointPort{{
							Name:     "http",
							Port:     int32(addr.Port),
							Protocol: v1.ProtocolTCP,
						}},
					}},
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
				es.Add(&v1.Endpoints{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Subsets: []v1.EndpointSubset{{
						Addresses: []v1.EndpointAddress{{
							IP: "127.0.0.1",
						}},
						NotReadyAddresses: []v1.EndpointAddress{{
							IP: "127.0.0.2",
						}},
						Ports: []v1.EndpointPort{{
							Name:     "http",
							Port:     int32(addr.Port),
							Protocol: v1.ProtocolTCP,
						}},
					}},
				})
			},
		}, {
			name:       "cds with eds endpoints (reverse order)",
			configFile: "envoy-cds.yaml",
			config:     dynamicConfig,
			push: func(addr *net.TCPAddr, es, cs cache.Store) {
				es.Add(&v1.Endpoints{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Service",
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test",
						Name:      "web",
					},
					Subsets: []v1.EndpointSubset{{
						Addresses: []v1.EndpointAddress{{
							IP: "127.0.0.1",
						}},
						NotReadyAddresses: []v1.EndpointAddress{{
							IP: "127.0.0.2",
						}},
						Ports: []v1.EndpointPort{{
							Name:     "http",
							Port:     int32(addr.Port),
							Protocol: v1.ProtocolTCP,
						}},
					}},
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

			// Serve a small HTTP page we can retrive through Envoy.
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
		})
	}
}

func redirectToLog(l *zap.Logger, cmd *exec.Cmd) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}
	go func() {
		s := bufio.NewScanner(stdout)
		l := l.Named("stdout")
		for s.Scan() {
			l.Info(s.Text())
		}
	}()
	go func() {
		s := bufio.NewScanner(stderr)
		l := l.Named("stderr")
		for s.Scan() {
			l.Info(s.Text())
		}
	}()
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
