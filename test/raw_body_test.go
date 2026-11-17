package test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
)

// A binary body (NUL bytes, invalid UTF-8, 1 MiB) reaches PHP and comes back
// byte for byte.
func TestBinaryBodyRoundTrip(t *testing.T) {
	body := make([]byte, 1<<20)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	body[0], body[1] = 0, 0xff
	resp, err := http.Post("http://"+httpAddr+"/echo-body", "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if resp.Header.Get("X-Body-Sha256") != hex.EncodeToString(sum[:]) {
		t.Errorf("PHP saw a different body: sha256 %s", resp.Header.Get("X-Body-Sha256"))
	}
	if !bytes.Equal(got, body) {
		t.Errorf("response body differs: %d bytes back, %d sent", len(got), len(body))
	}
}
