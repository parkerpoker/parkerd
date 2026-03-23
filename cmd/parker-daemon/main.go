package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	parker "github.com/danieldresner/arkade_fun/internal"
	daemonpkg "github.com/danieldresner/arkade_fun/internal/daemon"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	flags := parker.ParseFlagsOnly(argv)
	profile, ok := parker.FlagString(flags, "profile")
	if !ok || profile == "" {
		return fmt.Errorf("--profile is required for daemon startup")
	}

	config, err := parker.ResolveRuntimeConfig(flags)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(os.Stderr, "go parker daemon starting")

	mode := "player"
	if value, ok := parker.FlagString(flags, "mode"); ok {
		switch value {
		case "host", "witness", "indexer", "player":
			mode = value
		}
	}

	daemon, err := daemonpkg.New(profile, config, mode)
	if err != nil {
		return err
	}
	if err := daemon.Start(); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	go func() {
		<-signals
		_ = daemon.Stop()
	}()

	daemon.Wait()
	return nil
}
