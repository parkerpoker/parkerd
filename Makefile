SHELL := /bin/bash

PARKER_BIN_DIR ?= .tmp/parker-bin

.PHONY: rebuild-binaries local local-down deps deps-down dealer dealer-down witness witness-down alice alice-down bob bob-down fund-bob fund-alice poker-regtest-round poker-regtest-round-rebuild poker-regtest-round-tor poker-regtest-round-rebuild-tor poker-regtest-round-host-player poker-regtest-round-host-player-rebuild poker-regtest-round-host-player-tor poker-regtest-round-host-player-rebuild-tor

rebuild-binaries:
	rm -rf "$(PARKER_BIN_DIR)"

local: rebuild-binaries
	./scripts/local-stack.sh local-up

local-down:
	./scripts/local-stack.sh local-down

deps:
	./scripts/local-stack.sh deps-up

deps-down:
	./scripts/local-stack.sh deps-down

dealer:
	./scripts/local-stack.sh start-daemon dealer

dealer-down:
	./scripts/local-stack.sh stop-daemon dealer

witness:
	./scripts/local-stack.sh start-daemon witness

witness-down:
	./scripts/local-stack.sh stop-daemon witness

alice:
	./scripts/local-stack.sh start-daemon alice

alice-down:
	./scripts/local-stack.sh stop-daemon alice

bob:
	./scripts/local-stack.sh start-daemon bob

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
