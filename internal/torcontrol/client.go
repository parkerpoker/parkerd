package torcontrol

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultDialTimeout = 5 * time.Second

type Client struct {
	ControlAddr string
	CookieAuth  string
	DialTimeout time.Duration
}

type HiddenServiceRequest struct {
	Key           string
	TargetAddress string
	VirtualPort   int
}

type HiddenService struct {
	Hostname      string
	PrivateKey    string
	ServiceID     string
	TargetAddress string
	VirtualPort   int
}

type ProtocolInfo struct {
	AuthMethods []string
	CookieFile  string
}

type controlReply struct {
	Code  int
	Lines []string
}

type controlConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (client Client) AddOnion(ctx context.Context, request HiddenServiceRequest) (HiddenService, error) {
	if strings.TrimSpace(request.TargetAddress) == "" {
		return HiddenService{}, fmt.Errorf("tor hidden service target address is required")
	}
	if request.VirtualPort <= 0 {
		return HiddenService{}, fmt.Errorf("tor hidden service virtual port is required")
	}

	control, err := client.connect(ctx)
	if err != nil {
		return HiddenService{}, err
	}
	defer control.close()

	if err := control.authenticate(client.CookieAuth); err != nil {
		return HiddenService{}, err
	}

	key := strings.TrimSpace(request.Key)
	if key == "" {
		key = "NEW:ED25519-V3"
	}
	reply, err := control.command(fmt.Sprintf("ADD_ONION %s Flags=Detach Port=%d,%s", key, request.VirtualPort, request.TargetAddress))
	if err != nil {
		return HiddenService{}, err
	}

	service, err := ParseAddOnionReply(reply.Lines)
	if err != nil {
		return HiddenService{}, err
	}
	if service.PrivateKey == "" && key != "NEW:ED25519-V3" {
		service.PrivateKey = key
	}
	service.TargetAddress = request.TargetAddress
	service.VirtualPort = request.VirtualPort
	return service, nil
}

func (client Client) DelOnion(ctx context.Context, serviceID string) error {
	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return nil
	}

	control, err := client.connect(ctx)
	if err != nil {
		return err
	}
	defer control.close()

	if err := control.authenticate(client.CookieAuth); err != nil {
		return err
	}
	reply, err := control.command(fmt.Sprintf("DEL_ONION %s", serviceID))
	if err != nil {
		return err
	}
	return ParseDelOnionReply(reply.Lines)
}

func (client Client) connect(ctx context.Context) (*controlConn, error) {
	addr := strings.TrimSpace(client.ControlAddr)
	if addr == "" {
		return nil, fmt.Errorf("tor control address is required")
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := client.DialTimeout
		if timeout <= 0 {
			timeout = defaultDialTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect tor control at %s: %w", addr, err)
	}
	return &controlConn{
		conn:   connection,
		reader: bufio.NewReader(connection),
	}, nil
}

func (control *controlConn) close() error {
	if control == nil || control.conn == nil {
		return nil
	}
	return control.conn.Close()
}

func (control *controlConn) authenticate(cookieAuth string) error {
	reply, err := control.command("PROTOCOLINFO 1")
	if err != nil {
		return err
	}
	info, err := ParseProtocolInfo(reply.Lines)
	if err != nil {
		return err
	}
	if !supportsCookieAuth(info.AuthMethods) {
		return fmt.Errorf("tor control does not advertise cookie authentication")
	}

	cookiePath, err := ResolveCookieAuthPath(cookieAuth, info.CookieFile)
	if err != nil {
		return err
	}
	cookie, err := os.ReadFile(cookiePath)
	if err != nil {
		return fmt.Errorf("read tor control cookie %s: %w", cookiePath, err)
	}

	reply, err = control.command("AUTHENTICATE " + strings.ToUpper(hex.EncodeToString(cookie)))
	if err != nil {
		return fmt.Errorf("authenticate with tor control cookie %s: %w", cookiePath, err)
	}
	if reply.Code != 250 {
		return fmt.Errorf("authenticate with tor control cookie %s: unexpected status %d", cookiePath, reply.Code)
	}
	return nil
}

func (control *controlConn) command(command string) (controlReply, error) {
	if _, err := fmt.Fprintf(control.conn, "%s\r\n", command); err != nil {
		return controlReply{}, err
	}
	reply, err := readControlReply(control.reader)
	if err != nil {
		return controlReply{}, err
	}
	if reply.Code != 250 {
		return controlReply{}, fmt.Errorf("tor control %d: %s", reply.Code, strings.Join(reply.Lines, "; "))
	}
	return reply, nil
}

func readControlReply(reader *bufio.Reader) (controlReply, error) {
	reply := controlReply{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return controlReply{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 {
			return controlReply{}, fmt.Errorf("invalid tor control reply line %q", line)
		}

		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return controlReply{}, fmt.Errorf("invalid tor control reply code in %q", line)
		}
		separator := line[3]
		payload := ""
		if len(line) > 4 {
			payload = line[4:]
		}

		if reply.Code == 0 {
			reply.Code = code
		}
		if reply.Code != code {
			return controlReply{}, fmt.Errorf("mixed tor control reply codes %d and %d", reply.Code, code)
		}

		reply.Lines = append(reply.Lines, payload)
		if separator == '+' {
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return controlReply{}, err
				}
				dataLine = strings.TrimRight(dataLine, "\r\n")
				if dataLine == "." {
					break
				}
				if strings.HasPrefix(dataLine, "..") {
					dataLine = dataLine[1:]
				}
				reply.Lines = append(reply.Lines, dataLine)
			}
		}
		if separator == ' ' {
			return reply, nil
		}
		if separator != '-' && separator != '+' {
			return controlReply{}, fmt.Errorf("invalid tor control reply separator %q", separator)
		}
	}
}

func ResolveCookieAuthPath(explicit, protocolCookieFile string) (string, error) {
	if path, explicitSet := explicitCookieAuthPath(explicit); explicitSet {
		if fileExists(path) {
			return path, nil
		}
		return "", fmt.Errorf("tor control cookie file %s does not exist", path)
	}

	candidates := make([]string, 0, 8)
	if strings.TrimSpace(protocolCookieFile) != "" {
		candidates = append(candidates, expandHome(strings.TrimSpace(protocolCookieFile)))
	}
	candidates = append(candidates, commonCookieAuthPaths()...)

	seen := map[string]struct{}{}
	checked := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		checked = append(checked, candidate)
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	if len(checked) == 0 {
		return "", fmt.Errorf("unable to resolve tor control cookie file")
	}
	return "", fmt.Errorf("unable to resolve tor control cookie file; checked %s", strings.Join(checked, ", "))
}

func explicitCookieAuthPath(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	switch strings.ToLower(trimmed) {
	case "1", "true", "yes", "auto":
		return "", false
	case "0", "false", "no":
		return "", false
	default:
		return expandHome(trimmed), true
	}
}

func commonCookieAuthPaths() []string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".tor", "control_auth_cookie"),
		filepath.Join(home, "Library", "Application Support", "Tor", "control_auth_cookie"),
		filepath.Join(home, "Library", "Application Support", "TorBrowser-Data", "Tor", "control_auth_cookie"),
		"/var/lib/tor/control_auth_cookie",
		"/usr/local/var/lib/tor/control_auth_cookie",
		"/opt/homebrew/var/lib/tor/control_auth_cookie",
	}
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func supportsCookieAuth(methods []string) bool {
	for _, method := range methods {
		switch strings.ToUpper(strings.TrimSpace(method)) {
		case "COOKIE", "SAFECOOKIE":
			return true
		}
	}
	return false
}
