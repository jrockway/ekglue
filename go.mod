module github.com/jrockway/ekglue

go 1.13

require (
	github.com/envoyproxy/go-control-plane v0.9.2
	github.com/go-test/deep v1.0.5
	github.com/gogo/protobuf v1.2.2-0.20190723190241-65acae22fc9d
	github.com/golang/protobuf v1.3.2
	github.com/grpc-ecosystem/go-grpc-middleware v1.1.0
	github.com/jrockway/opinionated-server v0.0.4
	github.com/miekg/dns v1.1.27
	github.com/opentracing/opentracing-go v1.1.1-0.20200124165624-2876d2018785
	github.com/prometheus/client_golang v1.3.0
	go.uber.org/zap v1.13.0
	google.golang.org/genproto v0.0.0-20190819201941-24fa4b261c55
	google.golang.org/grpc v1.27.0
	k8s.io/api v0.17.2
	k8s.io/apimachinery v0.17.2
	k8s.io/client-go v0.17.2
	k8s.io/klog v1.0.0
	sigs.k8s.io/yaml v1.1.0
)
