package torcontrol

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseProtocolInfo(lines []string) (ProtocolInfo, error) {
	info := ProtocolInfo{}
	for _, line := range lines {
		if !strings.HasPrefix(line, "AUTH ") {
			continue
		}
		fields, err := parseControlFields(strings.TrimPrefix(line, "AUTH "))
		if err != nil {
			return ProtocolInfo{}, err
		}
		if methods := strings.TrimSpace(fields["METHODS"]); methods != "" {
			info.AuthMethods = splitAndTrim(methods, ",")
		}
		info.CookieFile = strings.TrimSpace(fields["COOKIEFILE"])
	}
	return info, nil
}

func ParseAddOnionReply(lines []string) (HiddenService, error) {
	service := HiddenService{}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "ServiceID="):
			service.ServiceID = strings.TrimSpace(strings.TrimPrefix(line, "ServiceID="))
		case strings.HasPrefix(line, "PrivateKey="):
			service.PrivateKey = strings.TrimSpace(strings.TrimPrefix(line, "PrivateKey="))
		}
	}
	if service.ServiceID == "" {
		return HiddenService{}, fmt.Errorf("tor ADD_ONION reply missing ServiceID")
	}
	service.Hostname = service.ServiceID + ".onion"
	return service, nil
}

func ParseDelOnionReply(lines []string) error {
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case "", "OK":
			continue
		default:
			return fmt.Errorf("unexpected DEL_ONION reply payload %q", line)
		}
	}
	return nil
}

func parseControlFields(line string) (map[string]string, error) {
	fields := map[string]string{}
	for {
		line = strings.TrimSpace(line)
		if line == "" {
			return fields, nil
		}

		equalIndex := strings.IndexByte(line, '=')
		if equalIndex <= 0 {
			return nil, fmt.Errorf("invalid tor control field %q", line)
		}
		key := line[:equalIndex]
		line = line[equalIndex+1:]

		value := ""
		if strings.HasPrefix(line, "\"") {
			end := 1
			escaped := false
			for end < len(line) {
				switch line[end] {
				case '\\':
					escaped = !escaped
				case '"':
					if !escaped {
						goto foundQuote
					}
					escaped = false
				default:
					escaped = false
				}
				end++
			}
			return nil, fmt.Errorf("unterminated tor control quoted value")
		foundQuote:
			quoted := line[:end+1]
			decoded, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, err
			}
			value = decoded
			line = line[end+1:]
		} else {
			spaceIndex := strings.IndexByte(line, ' ')
			if spaceIndex < 0 {
				value = line
				line = ""
			} else {
				value = line[:spaceIndex]
				line = line[spaceIndex+1:]
			}
		}
		fields[key] = value
	}
}

func splitAndTrim(value string, separator string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, separator)
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}
