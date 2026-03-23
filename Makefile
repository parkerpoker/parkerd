SHELL := /bin/bash

.PHONY: poker-regtest-round dev-local poker-regtest-ui-2p

poker-regtest-round:
	./scripts/run-regtest-round.sh

dev-local:
	./scripts/bin/dev-local

poker-regtest-ui-2p:
	./scripts/run-two-player-ui.sh
