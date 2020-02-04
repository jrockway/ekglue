# ekglue - Envoy/Kubernetes glue

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
discovery API server allows us to provide Envoy with locality information, to support locality-aware
load balancing. This can save you money on inter-AZ network traffic, and save your users time with
lower network latency after their request arrives at your front proxy.
