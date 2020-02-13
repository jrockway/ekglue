FROM golang:1.13-alpine AS build
RUN apk add git bzr gcc musl-dev
WORKDIR /ekglue
COPY go.mod go.sum /ekglue/
RUN go mod download

COPY . /ekglue/
RUN go install ./cmd/ekglue

FROM alpine:latest
RUN apk add ca-certificates tzdata
ADD  https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.3.1/grpc_health_probe-linux-amd64 /bin/grpc_health_probe
RUN chmod a+x /bin/grpc_health_probe
WORKDIR /
COPY --from=build /go/bin/ekglue /go/bin/ekglue
CMD ["/go/bin/ekglue"]
