//go:build darwin && cgo

package devicekey

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// The real enclave, when this Mac has one (CI's macOS runners are VMs
// without it: there the shim must fail cleanly and New must fall back).
func TestSecureEnclaveShim(t *testing.T) {
	blob, point, errMsg := seBackend{}.create()
	if errMsg != "" {
		if !strings.HasPrefix(errMsg, "secure enclave: ") || len(blob) != 0 {
			t.Fatalf("create failure = %q (blob %d bytes)", errMsg, len(blob))
		}
		t.Logf("no usable Secure Enclave here: %s", errMsg)
		if k, err := New(time.Now()); err != nil || k.Hardware() {
			t.Fatalf("fallback = %+v, %v", k, err)
		}
	} else {
		t.Logf("enclave key: blob %d bytes, public point %d bytes", len(blob), len(point))
		start := time.Now()
		k, err := New(time.Now())
		if err != nil || !k.Hardware() {
			t.Fatalf("New = %+v, %v", k, err)
		}
		t.Logf("New (create + self-check) took %s", time.Since(start))
		msg := Message("POST", "/api/v1/agent/runner/heartbeat", 1700000000, "rt1")
		start = time.Now()
		sig, err := k.Sign(msg)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("one signature took %s", time.Since(start))
		pub, _ := x509.ParsePKIXPublicKey(k.publicKey)
		sum := sha256.Sum256(msg)
		if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig) {
			t.Fatal("Go doesn't verify the enclave's signature")
		}
		file := t.TempDir() + "/device-key.json"
		if err := k.Save(file); err != nil {
			t.Fatal(err)
		}
		back, err := Load(file)
		if err != nil || back.Check() != nil {
			t.Fatalf("reloaded enclave key: %v", err)
		}
	}
	// A blob this enclave didn't wrap never signs.
	if sig, errMsg := (seBackend{}).sign([]byte("not a key"), []byte("m")); errMsg == "" || len(sig) != 0 {
		t.Fatalf("bogus blob signed: %q", errMsg)
	}
}
