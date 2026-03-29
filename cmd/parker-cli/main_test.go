package main

import (
	"bytes"
	"os"
	"testing"

	parker "github.com/parkerpoker/parkerd/internal"
)

func TestBuildBootstrapParamsIncludesWalletNsec(t *testing.T) {
	command, flags, positionals := parker.ParseCommandArgv([]string{
		"bootstrap",
		"Alice",
		"--wallet-nsec",
		"nsec1testvalue",
	})
	if command != "bootstrap" {
		t.Fatalf("expected bootstrap command, received %q", command)
	}

	params := buildBootstrapParams(positionals, flags)
	if params["nickname"] != "Alice" {
		t.Fatalf("expected nickname Alice, received %#v", params["nickname"])
	}
	if params["walletNsec"] != "nsec1testvalue" {
		t.Fatalf("expected wallet nsec to be forwarded unchanged, received %#v", params["walletNsec"])
	}
}

func TestPrintHelpMentionsWalletNsecCommand(t *testing.T) {
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = originalStdout
	})

	printHelp()

	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	var output bytes.Buffer
	if _, err := output.ReadFrom(reader); err != nil {
		t.Fatalf("read help output: %v", err)
	}
	if !bytes.Contains(output.Bytes(), []byte("wallet [nsec|summary|deposit")) {
		t.Fatalf("expected help to mention wallet nsec command, got %q", output.String())
	}
}

func TestResolveTableVisibilityAcceptsExplicitVisibility(t *testing.T) {
	visibility, err := resolveTableVisibility(parker.FlagMap{
		"visibility": "private",
	})
	if err != nil {
		t.Fatalf("resolve visibility: %v", err)
	}
	if visibility != "private" {
		t.Fatalf("expected private visibility, got %q", visibility)
	}
}

func TestResolveTableVisibilityRejectsConflictingFlags(t *testing.T) {
	_, err := resolveTableVisibility(parker.FlagMap{
		"visibility": "private",
		"public":     "true",
	})
	if err == nil {
		t.Fatal("expected visibility conflict error")
	}
}
