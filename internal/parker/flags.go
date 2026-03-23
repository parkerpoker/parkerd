package parker

import "strings"

type FlagMap map[string]string

func ParseCommandArgv(argv []string) (string, FlagMap, []string) {
	flags := FlagMap{}
	if len(argv) == 0 {
		return "", flags, nil
	}

	command := argv[0]
	positionals := make([]string, 0, len(argv))
	for index := 1; index < len(argv); index++ {
		value := argv[index]
		if !strings.HasPrefix(value, "--") {
			positionals = append(positionals, value)
			continue
		}

		key, parsedValue, consumed := parseFlag(argv, index)
		flags[key] = parsedValue
		index += consumed
	}

	return command, flags, positionals
}

func ParseFlagsOnly(argv []string) FlagMap {
	flags := FlagMap{}
	for index := 0; index < len(argv); index++ {
		value := argv[index]
		if !strings.HasPrefix(value, "--") {
			continue
		}

		key, parsedValue, consumed := parseFlag(argv, index)
		flags[key] = parsedValue
		index += consumed
	}
	return flags
}

func FlagString(flags FlagMap, name string) (string, bool) {
	value, ok := flags[name]
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

func FlagBool(flags FlagMap, name string) bool {
	value, ok := flags[name]
	if !ok {
		return false
	}
	if value == "" {
		return true
	}
	return parseBoolean(value, false)
}

func parseFlag(argv []string, index int) (string, string, int) {
	value := argv[index]
	keyValue := strings.SplitN(strings.TrimPrefix(value, "--"), "=", 2)
	key := keyValue[0]
	if len(keyValue) == 2 {
		return key, keyValue[1], 0
	}

	if index+1 >= len(argv) || strings.HasPrefix(argv[index+1], "--") {
		return key, "true", 0
	}

	return key, argv[index+1], 1
}
