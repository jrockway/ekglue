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

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	envoy_api_v2_endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/golang/protobuf/ptypes"
	"github.com/jrockway/ekglue/pkg/cds"
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
		ctx, c := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer c()
		req = req.WithContext(ctx)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("ping envoy: attempt %d: %v", i, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if got, want := res.StatusCode, http.StatusOK; got != want {
			t.Logf("ping envoy: attempt %d: status code %d", i, got)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		ok = true
		break
	}
	if !ok {
		return errors.New("failed after 20 attempts")
	}
	return nil
}

func TestCDS(t *testing.T) {
	// Find the Envoy binary.
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

	// Serve a small HTTP page we can retrive through Envoy.
	hl, err := net.Listen("tcp", "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gotReqCh := make(chan struct{})
	go http.Serve(hl, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello, world!"))
		go func() { gotReqCh <- struct{}{} }()
	}))

	// Serve the xDS RPC service, to allow Envoy to discover the server above.
	gl, err := net.Listen("tcp", "127.0.0.1:9090")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	go gs.Serve(gl)

	cds := cds.NewServer("cds-test-")
	envoy_api_v2.RegisterClusterDiscoveryServiceServer(gs, cds)

	// Start Envoy.
	cmd := exec.Command(envoy, "-c", "envoy.yaml", "-l", "warning")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start envoy: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for Envoy to start up.
	if err := get(t, "http://localhost:9091/ping"); err != nil {
		t.Fatalf("envoy never started: %v", err)
	}

	// Push the location of our webserver to Envoy.
	httpPort := hl.Addr().(*net.TCPAddr).Port
	cds.AddClusters([]*envoy_api_v2.Cluster{{
		Name:                 "test",
		ConnectTimeout:       ptypes.DurationProto(time.Second),
		ClusterDiscoveryType: &envoy_api_v2.Cluster_Type{Type: envoy_api_v2.Cluster_STATIC},
		LoadAssignment: &envoy_api_v2.ClusterLoadAssignment{
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
		},
	}})

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
}
