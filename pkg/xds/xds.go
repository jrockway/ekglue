// Package xds implements a CDS and EDS server.
package xds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/jrockway/opinionated-server/server"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// cdsSession represents an RPC stream subscribed to cluster updates.
type cdsSession chan struct{}

// Server receives updates asynchronously via AddCluster
type Server struct {
	sync.Mutex

	// We do not implement the GRPC_DELTA or REST protocols.  We include this to pick up stubs
	// for those methods, and any future protocols that are added.
	envoy_api_v2.UnimplementedClusterDiscoveryServiceServer

	cdsVersion  int
	clusters    map[string]*envoy_api_v2.Cluster
	cdsSessions map[cdsSession]struct{}

	// ackCh is only for tests
	ackCh chan struct{}
}

// NewServer returns a new server that is ready to serve.
func NewServer() *Server {
	return &Server{
		clusters:    make(map[string]*envoy_api_v2.Cluster),
		cdsSessions: make(map[cdsSession]struct{}),
	}
}

// AddClusters adds or updates clusters, and notifies all connected clients of the change.
func (s *Server) AddClusters(cs []*envoy_api_v2.Cluster) error {
	if len(cs) == 0 {
		return nil
	}

	var validationErrors []error
	for i, c := range cs {
		if err := c.Validate(); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("cluster %q (element %d): %w", c.GetName(), i, err))
		}
	}
	if n := len(validationErrors); n > 0 {
		return fmt.Errorf("%d validation error(s): %v", n, validationErrors)
	}

	s.Lock()
	defer s.Unlock()

	for _, c := range cs {
		name := c.GetName()
		_, overwrote := s.clusters[name]
		if overwrote {
			zap.L().Info("updating existing cluster", zap.String("cluster_name", name))
		} else {
			zap.L().Info("adding new cluster", zap.String("cluster_name", name))
		}
		s.clusters[name] = c
	}
	clusterUpdateCount.Add(float64(len(cs)))
	s.cdsVersion++
	clusterConfigVersions.With(prometheus.Labels{"config_version": strconv.Itoa(s.cdsVersion)}).SetToCurrentTime()

	for session := range s.cdsSessions {
		select {
		case session <- struct{}{}:
			clusterUpdateSessionsInformed.Inc()
		default:
			clusterUpdateSessionsMissed.Inc()
			zap.L().Warn("cluster update would have blocked; skipping", zap.Any("session", session))
		}
	}
	return nil
}

// snapshotClusters returns a copy of all the currently-tracked clusters and the version number of
// the resulting config.  You must hold the lock on Server to call this method.
func (s *Server) snapshotClusters() (int, []*any.Any) {
	result := make([]*any.Any, 0, len(s.clusters))
	for _, cluster := range s.clusters {
		any, err := ptypes.MarshalAny(cluster)
		if err != nil {
			zap.L().Fatal("marshal cluster to any", zap.Any("cluster", cluster), zap.Error(err))
		}
		result = append(result, any)
	}
	return s.cdsVersion, result
}

// buildDiscoveryResponse builds a validated DiscoveryResponse containing clusters.  We do our own
// validation, to avoid being surprised about Envoy rejecting an update.
func buildDiscoveryResponse(version int, typeURL string, resources []*any.Any) (*envoy_api_v2.DiscoveryResponse, error) {
	res := &envoy_api_v2.DiscoveryResponse{
		VersionInfo: strconv.Itoa(version),
		TypeUrl:     typeURL,
		Resources:   resources,
		Nonce:       fmt.Sprintf("ekglue-%d", version),
	}
	if err := res.Validate(); err != nil {
		return nil, fmt.Errorf("validating generated discovery response: %w", err)
	}
	return res, nil
}

// StreamClusters implements CDS.
func (s *Server) StreamClusters(stream envoy_api_v2.ClusterDiscoveryService_StreamClustersServer) error {
	ctx := stream.Context()
	l := ctxzap.Extract(ctx)

	reqCh := make(chan *envoy_api_v2.DiscoveryRequest)
	errCh := make(chan error)
	clustersCh := make(cdsSession)

	cdsClientsStreaming.Inc()
	defer cdsClientsStreaming.Dec()

	s.Lock()
	s.cdsSessions[clustersCh] = struct{}{}
	s.Unlock()
	defer func() {
		s.Lock()
		delete(s.cdsSessions, clustersCh)
		close(clustersCh)
		s.Unlock()
	}()

	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				close(errCh)
				close(reqCh)
				return
			}
			reqCh <- req
		}
	}()

	var node string
	pushState := make(map[string]string)
	push := func() error {
		s.Lock()
		version, clusters := s.snapshotClusters()
		s.Unlock()
		res, err := buildDiscoveryResponse(version, "type.googleapis.com/envoy.api.v2.Cluster", clusters)
		if err != nil {
			return fmt.Errorf("build discovery response: validate: %w", err)
		}
		if err := stream.Send(res); err != nil {
			return fmt.Errorf("send discovery response: %w", err)
		}
		pushState[res.GetNonce()] = res.GetVersionInfo()
		return nil
	}

	for {
		select {
		case err := <-errCh:
			// End because recv errored.
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("receiving message from stream: %w", err)
			}
			return nil

		case <-ctx.Done():
			// End because the context expired.
			err := ctx.Err()
			l.Debug("context done", zap.Error(err))
			if errors.Is(err, context.DeadlineExceeded) {
				return status.Error(codes.DeadlineExceeded, err.Error())
			} else if errors.Is(err, context.Canceled) {
				return status.Error(codes.Canceled, err.Error())
			}
			return status.Error(codes.Unknown, err.Error())

		case <-server.Draining():
			// End because the server is shutting down.
			return status.Error(codes.Unavailable, "server draining")

		case req := <-reqCh:
			// Process a message from Envoy.  An empty nonce means this is a new stream
			// and they want a config push.  A nonce means that it's an ACK or NACK for
			// a config we already pushed.
			if node == "" {
				// According to the Envoy docs, it will only send this information
				// once per stream.
				node = req.GetNode().GetId()
				l = l.With(zap.String("envoy.node.id", node))
			}
			if t := req.GetTypeUrl(); t != "type.googleapis.com/envoy.api.v2.Cluster" {
				// Ignore xDS requests that aren't for clusters.
				l.Info("ignoring request with non-cluster type_url", zap.String("request.type_url", t))
				break
			}
			version := req.GetVersionInfo()
			nonce := req.GetResponseNonce()
			if origVersion, ok := pushState[nonce]; ok {
				if s.ackCh != nil {
					// Notify tests that an ack/nack was received.
					s.ackCh <- struct{}{}
				}
				if err := req.GetErrorDetail(); err != nil {
					l.Error("envoy rejected configuration", zap.Any("error", err), zap.String("version.rejected", origVersion), zap.String("version.in_use", version))
					clusterConfigAcceptanceStatus.With(prometheus.Labels{"config_version": origVersion, "status": "NACK"}).Inc()
					break
				}
				l.Info("envoy accepted configuration", zap.String("version.in_use", version), zap.String("version.sent", origVersion))
				clusterConfigAcceptanceStatus.With(prometheus.Labels{"config_version": origVersion, "status": "ACK"}).Inc()
				break
			}

			if nonce != "" || version != "" {
				err := req.GetErrorDetail()
				l.Warn("envoy sent acknowlegement for a version that we are not tracking; resending config", zap.String("version.in_use", version), zap.String("nonce", nonce), zap.Any("error", err))
			}

			l.Info("sending initial cluster list")
			if err := push(); err != nil {
				l.Error("pushing clusters failed", zap.Error(err))
				// If pushing fails, we want to kill the stream so that Envoy knows
				// something is wrong.
				return fmt.Errorf("pushing clusters: %w", err)
			}
		case <-clustersCh:
			l.Info("clusters changed; sending update")
			if err := push(); err != nil {
				return fmt.Errorf("pushing clusters: %w", err)
			}
			break
		}
	}
}
