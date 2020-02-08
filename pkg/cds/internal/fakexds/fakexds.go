package fakexds

import (
	"context"
	"fmt"

	envoy_api_v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"google.golang.org/grpc/metadata"
)

type Stream struct {
	ctx   context.Context
	reqCh chan *envoy_api_v2.DiscoveryRequest
	resCh chan *envoy_api_v2.DiscoveryResponse
}

func NewStream(ctx context.Context) *Stream {
	return &Stream{
		ctx:   ctx,
		reqCh: make(chan *envoy_api_v2.DiscoveryRequest),
		resCh: make(chan *envoy_api_v2.DiscoveryResponse),
	}
}

func (s *Stream) Context() context.Context {
	return s.ctx
}

// Send is called by the server to send a response to the client.
func (s *Stream) Send(res *envoy_api_v2.DiscoveryResponse) error {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.resCh <- res:
		return nil
	}
}

// Recv is called by the server to receive a request from the client.
func (s *Stream) Recv() (*envoy_api_v2.DiscoveryRequest, error) {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case req := <-s.reqCh:
		return req, nil
	}
}

// Request is called by the client to send a message to the server.
func (s *Stream) Request(req *envoy_api_v2.DiscoveryRequest) error {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return fmt.Errorf("sending request: %w", ctx.Err())
	case s.reqCh <- req:
		return nil
	}
}

// Await is called by the client to await a push from the server.
func (s *Stream) Await() (*envoy_api_v2.DiscoveryResponse, error) {
	ctx := s.Context()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("receiving response: %w", ctx.Err())
	case res := <-s.resCh:
		return res, nil
	}
}

// RequestAndWait is called by the client to send a request to the server and wait for a response.
func (s *Stream) RequestAndWait(req *envoy_api_v2.DiscoveryRequest) (*envoy_api_v2.DiscoveryResponse, error) {
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
