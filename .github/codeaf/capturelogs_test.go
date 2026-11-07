package main

import (
	"bytes"
	"log"
	"testing"
)

// captureLogs redirects the package log into a buffer until the test ends
// (restored via t.Cleanup: a defer inside this helper would fire when the
// helper returns, not when the test does). Use it wherever the proxy is
// expected to log, to keep the refusal noise out of the test output — and
// where the log itself is under test. It moved here from the deleted
// search_proxy_test.go (issue #2); its users are elsewhere.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}
