module github.com/jrockway/ekglue

go 1.13

require (
	github.com/envoyproxy/go-control-plane v0.9.9
	github.com/go-test/deep v1.0.5
	github.com/google/go-cmp v0.5.6
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0
	github.com/jrockway/opinionated-server v0.0.22-0.20210818123348-97ea3e8bcffd
	github.com/miekg/dns v1.1.43
	github.com/opentracing/opentracing-go v1.2.0
	github.com/prometheus/client_golang v1.11.0
	github.com/stretchr/objx v0.2.0 // indirect
	github.com/uber/jaeger-client-go v2.29.1+incompatible
	go.uber.org/zap v1.19.1
	google.golang.org/genproto v0.0.0-20201019141844-1ed22bb0c154
	google.golang.org/grpc v1.40.0
	google.golang.org/protobuf v1.27.1
	k8s.io/api v0.22.2
	k8s.io/apimachinery v0.22.2
	k8s.io/client-go v0.22.2
	k8s.io/klog v1.0.0
	sigs.k8s.io/yaml v1.2.0
)
