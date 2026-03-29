package config

import "path/filepath"

func ResolveLauncherPath(scriptName string) (string, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "scripts", "bin", scriptName), nil
}
