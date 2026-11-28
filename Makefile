GO ?= go
COMPOSE ?= docker compose
COURSE = $(GO) run ./cmd/course
LAB = $(COMPOSE) run --rm lab
PACKAGE ?= ./...
experiment = $(wildcard ./ch*/experiments/$(EXP)-*)

.PHONY: help build test vet run run-native fetch-source build-tools check site \
        docker-build docker-run docker-run-raw docker-shell docker-test docker-check \
        docker-up docker-ps docker-logs docker-down require-experiment

help:
	@printf '%s\n' \
	  'Docker lab' \
	  '  make docker-build                             Build the lab image' \
	  '  make docker-run EXP=3.1 [ARGS=-i]             Run an experiment with its DuckDB report' \
	  '  make docker-run-raw EXP=7.4 ARGS=-s3           Run an experiment in the lab with its own flags' \
	  '  make docker-shell                             Open a shell in the lab' \
	  '  make docker-test                               Run Go tests in the lab' \
	  '  make docker-check ARGS="evidence ch03"         Check evidence in the lab' \
	  '  make docker-up                                Start SeaweedFS for chapter 10 and 7.4 with -s3' \
	  '  make docker-ps                                Show Compose service status' \
	  '  make docker-logs                              Show SeaweedFS logs' \
	  '  make docker-down                              Stop Compose services and keep their volumes' \
	  '' \
	  'Go on the host' \
	  '  make build                                    Compile all Go packages' \
	  '  make test [PACKAGE=./internal/walfile]          Run Go tests' \
	  '  make vet                                      Run go vet on all Go packages' \
	  '  make run EXP=3.1 [ARGS=-i]                     Launch the Docker lab via the host Go runner' \
	  '  make run-native EXP=3.1                        Run an experiment directly with Go' \
	  '  make fetch-source                             Fetch and verify the pinned SQLite source' \
	  '  make build-tools                              Build the SQLite C tools after fetch-source' \
	  '  make check [ARGS="spans ch03"]                 Check the repo using the pinned SQLite source' \
	  '  make site [ARGS="-check -base /walfleet/"]      Build or verify the course site' \
	  '' \
	  '  check and docker-check default to all -skip-evidence.'

build:
	$(GO) build ./...

test:
	$(GO) test $(ARGS) $(PACKAGE)

vet:
	$(GO) vet ./...

fetch-source:
	$(COURSE) fetch-source $(ARGS)

build-tools:
	$(COURSE) build-tools $(ARGS)

check:
	$(COURSE) check $(or $(ARGS),all -skip-evidence)

site:
	$(COURSE) site $(ARGS)

run run-native docker-run docker-run-raw: require-experiment

require-experiment:
	$(if $(and $(EXP),$(filter 1,$(words $(experiment)))),,@printf '%s\n' 'Set EXP to one experiment number, such as EXP=3.1.' >&2; exit 2)

run:
	$(COURSE) run $(ARGS) "$(EXP)"

run-native:
	$(GO) run $(experiment) $(ARGS)

docker-build:
	$(COMPOSE) build lab

docker-run:
	$(LAB) $(COURSE) run $(ARGS) "$(EXP)"

docker-run-raw:
	$(LAB) $(GO) run $(experiment) $(ARGS)

docker-shell:
	$(LAB)

docker-test:
	$(COMPOSE) run --rm -T lab $(GO) test $(ARGS) $(PACKAGE)

docker-check:
	$(COMPOSE) run --rm -T lab $(COURSE) check $(or $(ARGS),all -skip-evidence)

docker-up:
	$(COMPOSE) up -d seaweedfs

docker-ps:
	$(COMPOSE) ps

docker-logs:
	$(COMPOSE) logs seaweedfs

docker-down:
	$(COMPOSE) down
