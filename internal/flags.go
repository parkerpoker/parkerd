package parker

import argvpkg "github.com/parkerpoker/parkerd/internal/argv"

type FlagMap = argvpkg.FlagMap

func ParseCommandArgv(argv []string) (string, FlagMap, []string) {
	return argvpkg.ParseCommandArgv(argv)
}

func ParseFlagsOnly(argv []string) FlagMap {
	return argvpkg.ParseFlagsOnly(argv)
}

func FlagString(flags FlagMap, name string) (string, bool) {
	return argvpkg.FlagString(flags, name)
}

func FlagBool(flags FlagMap, name string) bool {
	return argvpkg.FlagBool(flags, name)
}
