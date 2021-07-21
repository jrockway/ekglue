module github.com/jrockway/ekglue

go 1.13

require (
	github.com/envoyproxy/go-control-plane v0.9.9
	github.com/go-test/deep v1.0.5
	github.com/google/go-cmp v0.5.4
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0
	github.com/jrockway/opinionated-server v0.0.21
	github.com/miekg/dns v1.1.43
	github.com/opentracing/opentracing-go v1.2.0
	github.com/prometheus/client_golang v1.7.1
	github.com/uber/jaeger-client-go v2.29.1+incompatible
	go.uber.org/zap v1.18.1
	google.golang.org/genproto v0.0.0-20200715011427-11fb19a81f2c
	google.golang.org/grpc v1.39.0
	google.golang.org/protobuf v1.25.0
	k8s.io/api v0.21.3
	k8s.io/apimachinery v0.21.3
	k8s.io/client-go v0.21.3
	k8s.io/klog v1.0.0
	sigs.k8s.io/yaml v1.2.0
)
