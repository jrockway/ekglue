module github.com/jrockway/ekglue

go 1.13

require (
	github.com/envoyproxy/go-control-plane v0.9.5
	github.com/go-test/deep v1.0.5
	github.com/golang/protobuf v1.4.2
	github.com/google/go-cmp v0.4.0
	github.com/grpc-ecosystem/go-grpc-middleware v1.2.0
	github.com/jrockway/opinionated-server v0.0.10
	github.com/miekg/dns v1.1.27
	github.com/opentracing/opentracing-go v1.1.1-0.20200124165624-2876d2018785
	github.com/prometheus/client_golang v1.4.1
	github.com/uber/jaeger-client-go v2.23.1+incompatible
	go.uber.org/zap v1.15.0
	google.golang.org/genproto v0.0.0-20200526211855-cb27e3aa2013
	google.golang.org/grpc v1.29.1
	google.golang.org/protobuf v1.24.0
	k8s.io/api v0.18.3
	k8s.io/apimachinery v0.18.3
	k8s.io/client-go v0.18.3
	k8s.io/klog v1.0.0
	sigs.k8s.io/yaml v1.2.0
)
