FROM golang:1.16.3-alpine3.13 as builder

ARG VERSION

RUN apk add --no-cache ca-certificates git bash gcc musl-dev

WORKDIR /opt/src
COPY events events
COPY registry registry
COPY *.go go.mod go.sum ./

RUN go test -v ./registry && \
    go build -ldflags="-s -w -X main.version=$VERSION" -o /opt/docker-registry-ui ./*.go


FROM alpine:3.13

WORKDIR /opt
RUN apk add --no-cache ca-certificates tzdata && \
    mkdir /opt/data && \
    chown nobody /opt/data

COPY templates /opt/templates
COPY static /opt/static
COPY --from=builder /opt/docker-registry-ui /opt/

USER nobody
ENTRYPOINT ["/opt/docker-registry-ui"]
