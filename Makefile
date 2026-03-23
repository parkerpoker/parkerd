SHELL := /bin/bash

.PHONY: poker-regtest-round dev-local poker-regtest-ui-2p

poker-regtest-round:
	./scripts/run-regtest-round.sh

dev-local:
	node --import tsx scripts/dev-local.ts

poker-regtest-ui-2p:
	./scripts/run-two-player-ui.sh
