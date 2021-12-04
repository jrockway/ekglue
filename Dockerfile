FROM golang:1.17 AS build
WORKDIR /ekglue
ADD  https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.4.1/grpc_health_probe-linux-amd64 /bin/grpc_health_probe
RUN chmod a+x /bin/grpc_health_probe

COPY go.mod go.sum /ekglue/
RUN go mod download

COPY . /ekglue/
RUN CGO_ENABLED=0 go install ./cmd/ekglue

FROM gcr.io/distroless/static-debian11
WORKDIR /
COPY --from=build /bin/grpc_health_probe /bin/grpc_health_probe
COPY --from=build /go/bin/ekglue /go/bin/ekglue
CMD ["/go/bin/ekglue"]
