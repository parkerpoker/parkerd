SHELL := /bin/bash

.PHONY: poker-regtest-round dev-local poker-regtest-ui-2p

PARKER_BIN_DIR ?= .tmp/parker-bin

.PHONY: rebuild-binaries poker-regtest-round-rebuild poker-regtest-ui-2p-rebuild dev-local-rebuild

rebuild-binaries:
	rm -rf "$(PARKER_BIN_DIR)"

poker-regtest-round:
	./scripts/run-regtest-round.sh

poker-regtest-round-rebuild: rebuild-binaries
	./scripts/run-regtest-round.sh

dev-local:
	./scripts/bin/dev-local

dev-local-rebuild: rebuild-binaries
	./scripts/bin/dev-local

poker-regtest-ui-2p:
	./scripts/run-two-player-ui.sh

poker-regtest-ui-2p-rebuild: rebuild-binaries
	./scripts/run-two-player-ui.sh
