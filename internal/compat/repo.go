package compat

import (
	"errors"
	"os"
	"path/filepath"
)

func FindRepoRoot() (string, error) {
	starts := make([]string, 0, 2)
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}

	for _, start := range starts {
		root, err := walkToRepoRoot(start)
		if err == nil {
			return root, nil
		}
	}

	return "", errors.New("could not resolve parker repo root")
}

func walkToRepoRoot(start string) (string, error) {
	current := start
	for {
		if fileExists(filepath.Join(current, "package.json")) && fileExists(filepath.Join(current, "apps/daemon/src/index.ts")) {
			return current, nil
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return "", errors.New("repo root not found")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
