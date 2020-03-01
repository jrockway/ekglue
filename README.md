# ekglue - Envoy/Kubernetes glue

[![codecov](https://codecov.io/gh/jrockway/ekglue/branch/master/graph/badge.svg)](https://codecov.io/gh/jrockway/ekglue)
[![CI](https://ci.jrock.us/api/v1/teams/main/pipelines/ekglue/jobs/tests/badge)](https://ci.jrock.us/teams/main/pipelines/ekglue/jobs/tests/)

<p align="center">
	<img src="img/logo.png" width="60%" align="center">
</p>

**Status: I'm currently using this in production, but it needs a tiny bit more cleanup before I push
the containers to dockerhub and write docs.**

This project exists to glue together [Kubernetes](https://kubernetes.io/) and
[Envoy](https://www.envoyproxy.io/), allowing Envoy to read Kubernetes
[services](https://kubernetes.io/docs/concepts/services-networking/service/) and endpoints as
[clusters](https://www.envoyproxy.io/docs/envoy/latest/configuration/upstream/upstream) (via CDS)
and endpoints (via
[EDS](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/service_discovery#arch-overview-service-discovery-types-eds)).

In simple cases, the default DNS provided by
[headless services](https://kubernetes.io/docs/concepts/services-networking/service/#headless-services)
is probably good enough for your Envoy configuration, and in complex cases, you should probably use
a full service mesh like [Istio](https://istio.io/). This exists for the medium-sized case, where
you are manually writing Envoy configurations, but want to grab your clusters from an API instead of
typing the details into your configuration. Beyond saving you typing, using a native xDS service
discovery API server allows us to provide Envoy with locality information in support of
locality-aware load balancing. This can save you money on inter-AZ network traffic, and save your
users time with lower network latency between your proxy and the backend.

## Known Limitations and Gotchas

I have not tested this with `ExternalService` type services. It "should" work. Depending on how
external the external service is, you might want to skip EDS and declare the cluster a `LOGICAL_DNS`
type. If the external service uses TLS, you may want to configure Envoy with a literal SNI to
present to the upstream server; otherwise it might serve you the wrong certificate or otherwise act
confused (my experience with proxying to AWS services is that they will do the right things, but log
a ton of errors while doing that, which may distract you from real issues). You will also need to
configure how Envoy verifies the upstream's certificate; by default, it will trust anything!

Things are going to work very strangely if you use different `port` and `targetPort` numbers in your
service definition. It will never work for headless services, and will not work in EDS mode.

Things are going to work very strangely if you use non-headless services. In CDS-only mode, your
connections will go through kube-proxy as you'd expect, but remember that Envoy typically only uses
one connection per upstream, so there will be no load balancing unless you explicitly configure your
cluster to create one connection per request. In any mode involving EDS, we bypass kube-proxy by
programming Envoy with the Pod endpoints directly. To avoid confusing yourself, I recommend not
using any plain services by setting clusterIP to None for all of them.

It is possible, with the right set of overrides on `default:kubernetes:443`, to end up with a route
to your Kubernetes API server authenticated automatically with the service account that runs Envoy.
You'd have to do a lot of work to make it happen, but all the tools are available. Don't bridge that
to the Internet or you will be mining a lot of cryptocoins in short order.
