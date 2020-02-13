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
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/cds"
	"github.com/jrockway/ekglue/pkg/xds"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
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

func loadAssignmentForTestServer(addr net.Addr) *envoy_api_v2.ClusterLoadAssignment {
	httpPort := addr.(*net.TCPAddr).Port
	return &envoy_api_v2.ClusterLoadAssignment{
		ClusterName: "test",
		Endpoints: []*envoy_api_v2_endpoint.LocalityLbEndpoints{{
			LbEndpoints: []*envoy_api_v2_endpoint.LbEndpoint{{
				HostIdentifier: &envoy_api_v2_endpoint.LbEndpoint_Endpoint{
					Endpoint: &envoy_api_v2_endpoint.Endpoint{
						Address: &envoy_api_v2_core.Address{
							Address: &envoy_api_v2_core.Address_SocketAddress{
								SocketAddress: &envoy_api_v2_core.SocketAddress{
									Address: "127.0.0.1",
									PortSpecifier: &envoy_api_v2_core.SocketAddress_PortValue{
										PortValue: uint32(httpPort),
									},
								},
							},
						},
					},
				},
			}},
		}},
	}
}

func TestXDS(t *testing.T) {
	testData := []struct {
		name, configFile string
		push             func(net.Addr, *cds.Server)
	}{
		{
			name:       "cds cluster with static endpoints",
			configFile: "envoy-cds.yaml",
			push: func(addr net.Addr, s *cds.Server) {
				// Push the location of our webserver to Envoy.
				s.AddClusters([]*envoy_api_v2.Cluster{{
					Name:                 "test",
					ConnectTimeout:       ptypes.DurationProto(time.Second),
					ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STATIC},
					LoadAssignment:       loadAssignmentForTestServer(addr),
				}})
			},
		},
		{
			name:       "eds endpoints from static cluster",
			configFile: "envoy-eds-only.yaml",
			push: func(addr net.Addr, s *cds.Server) {
				// TODO(jrockway): This isn't an amazingly useful test; should
				// rewrite to go thru the glue layer.
				s.Endpoints.Add([]xds.Resource{
					loadAssignmentForTestServer(addr),
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
			gs := grpc.NewServer()
			go gs.Serve(gl)
			defer func() {
				gs.Stop()
				gl.Close()
			}()

			server := cds.NewServer("test-")
			envoy_api_v2.RegisterClusterDiscoveryServiceServer(gs, server)
			envoy_api_v2.RegisterEndpointDiscoveryServiceServer(gs, server)

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
			test.push(hl.Addr(), server)

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
