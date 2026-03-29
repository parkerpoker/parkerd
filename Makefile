SHELL := /bin/bash

PARKER_BIN_DIR ?= .tmp/parker-bin
HOST_PROFILE ?= alice

.PHONY: rebuild-binaries local local-down deps deps-down host host-down witness witness-down alice alice-down bob bob-down fund-bob fund-alice poker-regtest-round poker-regtest-round-rebuild poker-regtest-round-tor poker-regtest-round-rebuild-tor poker-regtest-round-host-player poker-regtest-round-host-player-rebuild poker-regtest-round-host-player-tor poker-regtest-round-host-player-rebuild-tor

rebuild-binaries:
	rm -rf "$(PARKER_BIN_DIR)"

local: rebuild-binaries
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh local-up

local-down:
	./scripts/local-stack.sh local-down

deps:
	./scripts/local-stack.sh deps-up

deps-down:
	./scripts/local-stack.sh deps-down

host:
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh start-daemon "$(HOST_PROFILE)"

host-down:
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh stop-daemon "$(HOST_PROFILE)"

witness:
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh start-daemon witness

witness-down:
	./scripts/local-stack.sh stop-daemon witness

alice:
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh start-daemon alice

alice-down:
	./scripts/local-stack.sh stop-daemon alice

bob:
	HOST_PROFILE="$(HOST_PROFILE)" ./scripts/local-stack.sh start-daemon bob

bob-down:
	./scripts/local-stack.sh stop-daemon bob

fund-bob:
	./scripts/local-stack.sh fund bob

fund-alice:
	./scripts/local-stack.sh fund alice

poker-regtest-round:
	./scripts/run-regtest-round.sh

poker-regtest-round-rebuild: rebuild-binaries
	./scripts/run-regtest-round.sh

poker-regtest-round-tor:
	USE_TOR=true ./scripts/run-regtest-round.sh

poker-regtest-round-rebuild-tor: rebuild-binaries
	USE_TOR=true ./scripts/run-regtest-round.sh

poker-regtest-round-host-player:
	ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh

poker-regtest-round-host-player-rebuild: rebuild-binaries
	ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh

poker-regtest-round-host-player-tor:
	USE_TOR=true ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh

poker-regtest-round-host-player-rebuild-tor: rebuild-binaries
	USE_TOR=true ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh
