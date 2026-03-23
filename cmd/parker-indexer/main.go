package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	parker "github.com/danieldresner/arkade_fun/internal"
	cfg "github.com/danieldresner/arkade_fun/internal/config"
	indexerpkg "github.com/danieldresner/arkade_fun/internal/indexer"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	flags := parker.ParseFlagsOnly(argv)
	runtimeConfig, err := cfg.ResolveRuntimeConfig(map[string]string(flags))
	if err != nil {
		return err
	}

	host := runtimeConfig.IndexerHost
	if value, ok := parker.FlagString(flags, "host"); ok && value != "" {
		host = value
	}
	port := runtimeConfig.IndexerPort
	if value, ok := parker.FlagString(flags, "port"); ok && value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil && parsed > 0 {
			port = parsed
		}
	}

	app, err := indexerpkg.NewApp(runtimeConfig)
	if err != nil {
		return err
	}
	defer app.Close()

	address := fmt.Sprintf("%s:%d", host, port)
	_, _ = fmt.Fprintf(os.Stdout, "parker indexer listening on http://%s\n", address)
	return http.ListenAndServe(address, app)
}
