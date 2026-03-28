SHELL := /bin/bash

PARKER_BIN_DIR ?= .tmp/parker-bin

.PHONY: rebuild-binaries poker-regtest-round poker-regtest-round-rebuild poker-regtest-round-tor poker-regtest-round-rebuild-tor

rebuild-binaries:
	rm -rf "$(PARKER_BIN_DIR)"

poker-regtest-round:
	./scripts/run-regtest-round.sh

poker-regtest-round-rebuild: rebuild-binaries
	./scripts/run-regtest-round.sh

poker-regtest-round-tor:
	USE_TOR=true ./scripts/run-regtest-round.sh

poker-regtest-round-rebuild-tor: rebuild-binaries
	USE_TOR=true ./scripts/run-regtest-round.sh
