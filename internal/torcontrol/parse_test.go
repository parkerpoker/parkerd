package torcontrol

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadControlReplyAndParseProtocolInfo(t *testing.T) {
	t.Parallel()

	reply, err := readControlReply(bufio.NewReader(strings.NewReader(
		"250-PROTOCOLINFO 1\r\n" +
			"250-AUTH METHODS=COOKIE,SAFECOOKIE COOKIEFILE=\"/tmp/tor/control_auth_cookie\"\r\n" +
			"250-VERSION Tor=\"0.4.8.12\"\r\n" +
			"250 OK\r\n",
	)))
	if err != nil {
		t.Fatalf("read control reply: %v", err)
	}
	info, err := ParseProtocolInfo(reply.Lines)
	if err != nil {
		t.Fatalf("parse protocol info: %v", err)
	}
	if info.CookieFile != "/tmp/tor/control_auth_cookie" {
		t.Fatalf("expected cookie file to be parsed, got %q", info.CookieFile)
	}
	if len(info.AuthMethods) != 2 || info.AuthMethods[0] != "COOKIE" || info.AuthMethods[1] != "SAFECOOKIE" {
		t.Fatalf("unexpected auth methods: %#v", info.AuthMethods)
	}
}

func TestParseAddAndDeleteOnionReplies(t *testing.T) {
	t.Parallel()

	addReply, err := readControlReply(bufio.NewReader(strings.NewReader(
		"250-ServiceID=merchantabcdefghijklmnop\r\n" +
			"250-PrivateKey=ED25519-V3:ABC123\r\n" +
			"250 OK\r\n",
	)))
	if err != nil {
		t.Fatalf("read add_onion reply: %v", err)
	}
	service, err := ParseAddOnionReply(addReply.Lines)
	if err != nil {
		t.Fatalf("parse add_onion reply: %v", err)
	}
	if service.Hostname != "merchantabcdefghijklmnop.onion" {
		t.Fatalf("expected onion hostname, got %q", service.Hostname)
	}
	if service.PrivateKey != "ED25519-V3:ABC123" {
		t.Fatalf("expected private key, got %q", service.PrivateKey)
	}

	delReply, err := readControlReply(bufio.NewReader(strings.NewReader("250 OK\r\n")))
	if err != nil {
		t.Fatalf("read del_onion reply: %v", err)
	}
	if err := ParseDelOnionReply(delReply.Lines); err != nil {
		t.Fatalf("parse del_onion reply: %v", err)
	}
}
