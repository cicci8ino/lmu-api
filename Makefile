DOCKER ?= docker
TAG ?= latest
REGISTRY_BASE_PATH ?= github.com/cicci8ino/lmu-api

IMAGE_FULL_PATH := $(REGISTRY_BASE_PATH):$(TAG)

.PHONY: build push

build-server:
	$(DOCKER) build --target server -t $(IMAGE_FULL_PATH) .

build: build-server

push-server:
	$(DOCKER) push $(IMAGE_FULL_PATH)

push: push-server

run-server:
	DEBUG=TRUE go run . 