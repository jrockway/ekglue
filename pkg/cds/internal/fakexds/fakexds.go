package fakexds

import (
	"context"
	"fmt"

	discovery_v3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc/metadata"
)

type Stream struct {
	ctx   context.Context
	reqCh chan *discovery_v3.DiscoveryRequest
	resCh chan *discovery_v3.DiscoveryResponse
}

func NewStream(ctx context.Context) *Stream {
	return &Stream{
		ctx:   ctx,
		reqCh: make(chan *discovery_v3.DiscoveryRequest),
		resCh: make(chan *discovery_v3.DiscoveryResponse),
	}
}

func (s *Stream) Context() context.Context {
	return s.ctx
}

// Send is called by the server to send a response to the client.
func (s *Stream) Send(res *discovery_v3.DiscoveryResponse) error {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.resCh <- res:
		return nil
	}
}

// Recv is called by the server to receive a request from the client.
func (s *Stream) Recv() (*discovery_v3.DiscoveryRequest, error) {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case req := <-s.reqCh:
		return req, nil
	}
}

// Request is called by the client to send a message to the server.
func (s *Stream) Request(req *discovery_v3.DiscoveryRequest) error {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return fmt.Errorf("sending request: %w", ctx.Err())
	case s.reqCh <- req:
		return nil
	}
}

// Await is called by the client to await a push from the server.
func (s *Stream) Await() (*discovery_v3.DiscoveryResponse, error) {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("receiving response: %w", ctx.Err())
	case res := <-s.resCh:
		return res, nil
	}
}

// RequestAndWait is called by the client to send a request to the server and wait for a response.
func (s *Stream) RequestAndWait(req *discovery_v3.DiscoveryRequest) (*discovery_v3.DiscoveryResponse, error) {
	if err := s.Request(req); err != nil {
		return nil, err
	}
	res, err := s.Await()
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Stream) RecvMsg(interface{}) error    { panic("unimplemented") }
func (s *Stream) SendMsg(interface{}) error    { panic("unimplemented") }
func (s *Stream) SendHeader(metadata.MD) error { return nil }
func (s *Stream) SetHeader(metadata.MD) error  { return nil }
func (s *Stream) SetTrailer(metadata.MD)       {}
