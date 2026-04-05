SHELL := /bin/bash

PARKER_BIN_DIR ?= .tmp/parker-bin
HOST_PROFILE ?= alice

.PHONY: rebuild-binaries local local-down deps deps-down host host-down witness witness-down alice alice-down bob bob-down fund-bob fund-alice kill-floating poker-regtest-round poker-regtest-round-tor poker-regtest-round-host-player poker-regtest-round-host-player-tor poker-regtest-round-recovery poker-regtest-round-aborted-hand poker-regtest-round-all-in poker-regtest-round-turn-challenge poker-regtest-round-emergency-exit poker-regtest-round-multi-hand poker-regtest-round-challenge-escape poker-regtest-round-recovery-showdown poker-regtest-round-cashout-after-challenge test-integration test-integration-tor

rebuild-binaries:
	rm -rf "$(PARKER_BIN_DIR)"

local: rebuild-binaries
	./scripts/local-stack.sh local-down
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

kill-floating:
	./scripts/kill-floating-parker-processes.sh

poker-regtest-round: rebuild-binaries kill-floating
	./scripts/run-regtest-round.sh

poker-regtest-round-tor: rebuild-binaries kill-floating
	USE_TOR=true ./scripts/run-regtest-round.sh

poker-regtest-round-host-player: rebuild-binaries kill-floating
	ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh

poker-regtest-round-host-player-tor: rebuild-binaries kill-floating
	USE_TOR=true ROUND_SCENARIO=host-player-2d ./scripts/run-regtest-round.sh

poker-regtest-round-recovery: rebuild-binaries kill-floating
	ROUND_SCENARIO=recovery-timeout-2d ./scripts/run-regtest-round.sh

poker-regtest-round-aborted-hand: rebuild-binaries kill-floating
	ROUND_SCENARIO=aborted-hand-2d ./scripts/run-regtest-round.sh

poker-regtest-round-all-in: rebuild-binaries kill-floating
	ROUND_SCENARIO=all-in-side-pot-2d ./scripts/run-regtest-round.sh

poker-regtest-round-turn-challenge: rebuild-binaries kill-floating
	ROUND_SCENARIO=turn-challenge-2d ./scripts/run-regtest-round.sh

poker-regtest-round-emergency-exit: rebuild-binaries kill-floating
	ROUND_SCENARIO=emergency-exit-2d ./scripts/run-regtest-round.sh

poker-regtest-round-multi-hand: rebuild-binaries kill-floating
	ROUND_SCENARIO=multi-hand-2d ./scripts/run-regtest-round.sh

poker-regtest-round-challenge-escape: rebuild-binaries kill-floating
	ROUND_SCENARIO=challenge-escape-2d ./scripts/run-regtest-round.sh

poker-regtest-round-recovery-showdown: rebuild-binaries kill-floating
	ROUND_SCENARIO=recovery-showdown-2d ./scripts/run-regtest-round.sh

poker-regtest-round-cashout-after-challenge: rebuild-binaries kill-floating
	ROUND_SCENARIO=cashout-after-challenge-2d ./scripts/run-regtest-round.sh

test-integration:
	go test -tags=integration ./internal/meshruntime -run TestRegtestRoundUsesRealArkCustody -count=1 -timeout 30m

test-integration-tor:
	USE_TOR=true go test -tags=integration ./internal/meshruntime -run TestRegtestRoundUsesRealArkCustody -count=1 -timeout 30m
