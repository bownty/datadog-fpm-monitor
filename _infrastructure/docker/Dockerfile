FROM golang:1.7
MAINTAINER Christian Winther <cw@bownty.com>

COPY . /go/src/github.com/bownty/datadog-fpm-monitor
WORKDIR /go/src/github.com/bownty/datadog-fpm-monitor

RUN make -j deploy-build-internal

WORKDIR /go
RUN rm -rf src/*
