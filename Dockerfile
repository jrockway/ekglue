FROM golang:1.20 AS build
WORKDIR /ekglue

COPY go.mod go.sum /ekglue/
RUN go mod download

COPY . /ekglue/
RUN CGO_ENABLED=0 go install ./cmd/ekglue

FROM gcr.io/distroless/static-debian11
WORKDIR /
COPY --from=build /go/bin/ekglue /go/bin/ekglue
CMD ["/go/bin/ekglue"]
