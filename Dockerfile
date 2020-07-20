FROM golang:1.14-alpine3.12 AS build
WORKDIR /ekglue
COPY go.mod go.sum /ekglue/
RUN go mod download

COPY . /ekglue/
RUN CGO_ENABLED=0 go install ./cmd/ekglue

FROM alpine:3.12
RUN apk add ca-certificates tzdata
ADD  https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.3.2/grpc_health_probe-linux-amd64 /bin/grpc_health_probe
RUN chmod a+x /bin/grpc_health_probe
ADD https://github.com/Yelp/dumb-init/releases/download/v1.2.2/dumb-init_1.2.2_x86_64 /usr/local/bin/dumb-init
RUN chmod +x /usr/local/bin/dumb-init
ENTRYPOINT ["/usr/local/bin/dumb-init"]
WORKDIR /
COPY --from=build /go/bin/ekglue /go/bin/ekglue
CMD ["/go/bin/ekglue"]
