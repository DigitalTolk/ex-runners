package devicekey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// fakeEnclave stands in for the Secure Enclave: its "wrapped blob" is a
// PKCS#8 software key, so the Go side runs exactly as with the real one.
type fakeEnclave struct {
	createErr, signErr string
	point              []byte // overrides the public point when set
	wrongSigner        bool   // signs with a different key than it reports
}

func (f fakeEnclave) create() ([]byte, []byte, string) {
	if f.createErr != "" {
		return nil, nil, f.createErr
	}
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	blob, _ := x509.MarshalPKCS8PrivateKey(priv)
	point, _ := priv.PublicKey.Bytes()
	if f.point != nil {
		point = f.point
	}
	if f.wrongSigner {
		other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		blob, _ = x509.MarshalPKCS8PrivateKey(other)
	}
	return blob, point, ""
}

func (f fakeEnclave) sign(blob, msg []byte) ([]byte, string) {
	if f.signErr != "" {
		return nil, f.signErr
	}
	priv, err := x509.ParsePKCS8PrivateKey(blob)
	if err != nil {
		return nil, "secure enclave: load key: bad blob"
	}
	sum := sha256.Sum256(msg)
	sig, _ := ecdsa.SignASN1(rand.Reader, priv.(*ecdsa.PrivateKey), sum[:])
	return sig, ""
}

func withEnclave(t *testing.T, h hardware) {
	t.Helper()
	prev := secureEnclave
	secureEnclave = h
	t.Cleanup(func() { secureEnclave = prev })
}

func verify(t *testing.T, k *Key, msg, sig []byte) bool {
	t.Helper()
	pub, err := x509.ParsePKIXPublicKey(k.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(msg)
	return ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig)
}

func TestNewPrefersAWorkingEnclave(t *testing.T) {
	withEnclave(t, fakeEnclave{})
	k, err := New(t0)
	if err != nil || !k.Hardware() || k.Kind() != KindSecureEnclave {
		t.Fatalf("New = %+v, %v", k, err)
	}
	sig, err := k.Sign([]byte("m"))
	if err != nil || !verify(t, k, []byte("m"), sig) {
		t.Fatalf("enclave signature: %v", err)
	}
	spki, _ := base64.StdEncoding.DecodeString(k.PublicKey())
	if pub, err := x509.ParsePKIXPublicKey(spki); err != nil || pub.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatalf("public key is not P-256 SPKI: %v", err)
	}

	// Every way the enclave can let us down ends in a software key.
	for name, h := range map[string]hardware{
		"none":         nil,
		"create fails": fakeEnclave{createErr: "secure enclave: create key: no SEP"},
		"bad point":    fakeEnclave{point: []byte{4, 1, 2}},
		"can't sign":   fakeEnclave{signErr: "secure enclave: sign: busy"},
		"wrong key":    fakeEnclave{wrongSigner: true},
	} {
		secureEnclave = h
		k, err := New(t0)
		if err != nil || k.Hardware() || k.Kind() != KindSoftware {
			t.Errorf("%s: New = %+v, %v", name, k, err)
		}
	}
}

func TestSoftwareKey(t *testing.T) {
	k, err := NewSoftware(t0)
	if err != nil || k.Hardware() {
		t.Fatalf("NewSoftware = %+v, %v", k, err)
	}
	sig, err := k.Sign([]byte("hello"))
	if err != nil || !verify(t, k, []byte("hello"), sig) || k.Check() != nil {
		t.Fatalf("software signature: %v", err)
	}

	generateKey = func(elliptic.Curve, io.Reader) (*ecdsa.PrivateKey, error) { return nil, errors.New("no entropy") }
	defer func() { generateKey = ecdsa.GenerateKey }()
	if _, err := NewSoftware(t0); err == nil {
		t.Error("key generation failure must surface")
	}
}

func TestSignAndCheckFailures(t *testing.T) {
	withEnclave(t, fakeEnclave{})
	k, _ := New(t0)

	secureEnclave = nil // the file was copied to a computer without the enclave
	if _, err := k.Sign([]byte("m")); !errors.Is(err, ErrNoSecureEnclave) {
		t.Errorf("no enclave = %v", err)
	}
	if k.Check() == nil {
		t.Error("Check must fail without the enclave")
	}
	secureEnclave = fakeEnclave{signErr: "secure enclave: sign: denied"}
	if _, err := k.Sign([]byte("m")); err == nil || err.Error() != "secure enclave: sign: denied" {
		t.Errorf("sign error = %v", err)
	}

	sw, _ := NewSoftware(t0)
	signASN1 = func(io.Reader, *ecdsa.PrivateKey, []byte) ([]byte, error) { return []byte{0x30, 0}, nil }
	defer func() { signASN1 = ecdsa.SignASN1 }()
	if err := sw.Check(); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("mismatch = %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withEnclave(t, fakeEnclave{})
	dir := filepath.Join(t.TempDir(), "home")
	for _, mk := range []func(time.Time) (*Key, error){New, NewSoftware} {
		k, _ := mk(t0)
		file := filepath.Join(dir, "device-key.json")
		if err := k.Save(file); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(file)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", info.Mode())
		}
		var f keyFile
		raw, _ := os.ReadFile(file)
		if json.Unmarshal(raw, &f) != nil || f.Kind != k.Kind() || f.PublicKey != k.PublicKey() || f.CreatedAt != "2026-10-28T12:00:00Z" {
			t.Errorf("file = %s", raw)
		}
		back, err := Load(file)
		if err != nil || back.Kind() != k.Kind() || back.PublicKey() != k.PublicKey() || back.Check() != nil {
			t.Errorf("Load = %+v, %v", back, err)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v", err)
	}
}

func TestSaveFailures(t *testing.T) {
	k, _ := NewSoftware(t0)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if k.Save(filepath.Join(blocker, "device-key.json")) == nil {
		t.Error("a file where the home should be")
	}
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o500)
	if k.Save(filepath.Join(ro, "device-key.json")) == nil {
		t.Error("unwritable home")
	}
	target := filepath.Join(dir, "target")
	_ = os.MkdirAll(filepath.Join(target, "x"), 0o700) // a non-empty dir can't be renamed over
	if k.Save(target) == nil {
		t.Error("rename over a directory")
	}
}

func TestLoadRefusesMalformedFiles(t *testing.T) {
	sw, _ := NewSoftware(t0)
	other, _ := NewSoftware(t0)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384spki, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaSPKI, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	b64 := base64.StdEncoding.EncodeToString
	good := keyFile{Kind: KindSoftware, Blob: b64(sw.blob), PublicKey: sw.PublicKey(), CreatedAt: "2026-10-28T12:00:00Z"}
	cases := map[string]func(f *keyFile){
		"kind":        func(f *keyFile) { f.Kind = "tpm" },
		"blob":        func(f *keyFile) { f.Blob = "%%%" },
		"empty blob":  func(f *keyFile) { f.Blob = "" },
		"public b64":  func(f *keyFile) { f.PublicKey = "%%%" },
		"public junk": func(f *keyFile) { f.PublicKey = b64([]byte("junk")) },
		"P-384":       func(f *keyFile) { f.PublicKey = b64(p384spki) },
		"RSA":         func(f *keyFile) { f.PublicKey = b64(rsaSPKI) },
		"createdAt":   func(f *keyFile) { f.CreatedAt = "yesterday" },
		"pkcs8 junk":  func(f *keyFile) { f.Blob = b64([]byte("junk")) },
		"other key":   func(f *keyFile) { f.Blob = b64(other.blob) },
		"rsa private": func(f *keyFile) { r, _ := x509.MarshalPKCS8PrivateKey(rsaKey); f.Blob = b64(r) },
	}
	dir := t.TempDir()
	for name, mutate := range cases {
		f := good
		mutate(&f)
		raw, _ := json.Marshal(f)
		file := filepath.Join(dir, "k.json")
		_ = os.WriteFile(file, raw, 0o600)
		if k, err := Load(file); err == nil || !strings.HasPrefix(err.Error(), "read k.json: ") {
			t.Errorf("%s: Load = %+v, %v", name, k, err)
		}
	}
	file := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(file, []byte("{broken"), 0o600)
	if _, err := Load(file); err == nil {
		t.Error("broken JSON must not load")
	}
	// An enclave key parses anywhere; it just can't sign without the enclave.
	ek := good
	ek.Kind, ek.Blob = KindSecureEnclave, b64([]byte("wrapped"))
	raw, _ := json.Marshal(ek)
	_ = os.WriteFile(file, raw, 0o600)
	if k, err := Load(file); err != nil || !k.Hardware() {
		t.Errorf("enclave file = %+v, %v", k, err)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	if err := Remove(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("missing = %v", err)
	}
	full := filepath.Join(dir, "full")
	_ = os.MkdirAll(filepath.Join(full, "x"), 0o700)
	if Remove(full) == nil {
		t.Error("a non-empty directory can't be removed")
	}
}

func TestProver(t *testing.T) {
	k, _ := NewSoftware(t0)
	p := NewProver(k, "rt1", func() time.Time { return t0 })
	proof, err := p.Proof("post", "/api/v1/agent/runner/heartbeat")
	parts := strings.Split(proof, ".")
	if err != nil || len(parts) != 3 || parts[0] != "v1" || parts[1] != "1793188800" {
		t.Fatalf("proof = %q, %v", proof, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	want := "ex-runner-proof/v1\nPOST\n/api/v1/agent/runner/heartbeat\n1793188800\nrt1"
	if err != nil || string(Message("post", "/api/v1/agent/runner/heartbeat", 1793188800, "rt1")) != want || !verify(t, k, []byte(want), sig) {
		t.Fatalf("signature doesn't verify over %q", want)
	}

	withEnclave(t, nil)
	ek := &Key{kind: KindSecureEnclave, blob: []byte("w"), publicKey: k.publicKey}
	if _, err := NewProver(ek, "rt1", time.Now).Proof("POST", "/x"); !errors.Is(err, ErrNoSecureEnclave) {
		t.Errorf("sign failure = %v", err)
	}
}
