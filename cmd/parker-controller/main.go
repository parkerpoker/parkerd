package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	parker "github.com/danieldresner/arkade_fun/internal"
	cfg "github.com/danieldresner/arkade_fun/internal/config"
	controllerpkg "github.com/danieldresner/arkade_fun/internal/controller"
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

	controllerPort := cfg.DefaultControllerPort
	if value, ok := parker.FlagString(flags, "port"); ok {
		controllerPort = parsePort(value, controllerPort)
	} else if raw := os.Getenv("PARKER_CONTROLLER_PORT"); raw != "" {
		controllerPort = parsePort(raw, controllerPort)
	}
	controllerHost := cfg.DefaultControllerHost
	if value, ok := parker.FlagString(flags, "host"); ok && value != "" {
		controllerHost = value
	} else if raw := os.Getenv("PARKER_CONTROLLER_HOST"); raw != "" {
		controllerHost = raw
	}

	repoRoot, err := cfg.FindRepoRoot()
	if err != nil {
		return err
	}
	webDistDir := filepath.Join(repoRoot, "apps", "web", "dist")
	app, err := controllerpkg.NewApp(controllerpkg.Options{
		Config:         runtimeConfig,
		ControllerPort: controllerPort,
		WebDistDir:     webDistDir,
	})
	if err != nil {
		return err
	}

	address := fmt.Sprintf("%s:%d", controllerHost, controllerPort)
	server := &http.Server{
		Addr:    address,
		Handler: app,
	}
	_, _ = fmt.Fprintf(os.Stdout, "parker controller listening on http://%s (allowed origins: %s)\n", address, strings.Join(app.AllowedOrigins(), ", "))
	return server.ListenAndServe()
}

func parsePort(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
