APP_ENV ?= development
APP_NAME ?= datadog-fpm-monitor
APP_VERSION ?= latest

.PHONY: install
install:
	go get github.com/kardianos/govendor

.PHONY: build
build: install
	govendor sync
	go install

.PHONY: deploy-build
deploy-build: deploy-docker-build deploy-docker-push

.PHONY: deploy-docker-build
deploy-docker-build:
	docker run \
		--rm \
		-v ${PWD}:/go/src/github.com/bownty/datadog-fpm-monitor \
		--net=host \
		golang:1.7-wheezy \
		bash -c "cd /go/src/github.com/bownty/datadog-fpm-monitor ; make deploy-build-internal"

.PHONY: deploy-docker-push
deploy-docker-push: deploy-docker-build
	curl \
		-X POST \
		-H "Authorization: ${GOOGLE_CLOUD_AUTH_KEY}" \
		"https://www.googleapis.com/upload/storage/v1/b/bownty-deploy-artifacts/o?uploadType=media&name=${APP_NAME}/${APP_ENV}/${APP_VERSION}/datadog-fpm-monitor" \
		--data-binary @datadog-fpm-monitor
	rm datadog-fpm-monitor

.PHONY: deploy-build-internal
deploy-build-internal: install build
	mv ${GOPATH}/bin/datadog-fpm-monitor .