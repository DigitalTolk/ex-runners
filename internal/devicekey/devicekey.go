// Package devicekey is this computer's device-bound key: a P-256 key whose
// private half never leaves the machine. `ex-runner login` registers its
// public half with the server, and every request made with the runner token
// carries a signature from it (X-Ex-Runner-Proof), so a credentials.json
// copied to another computer is useless there.
//
// On a Mac with a Secure Enclave the private key lives in the enclave: what
// reaches the disk is a wrapped blob only this Mac's enclave can use. The key
// is never added to the Keychain (no prompts, no entitlements, nothing any
// other app has to allow). Everywhere else — Linux, Windows, Macs without an
// enclave, VMs, CGO_ENABLED=0 builds — it is a software key in a file only
// the owner can read.
package devicekey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kinds of key, as written to device-key.json.
const (
	KindSecureEnclave = "secure-enclave"
	KindSoftware      = "software"
)

// hardware is the Secure Enclave, behind a seam: the cgo shim on macOS
// (se_darwin.go), nil everywhere else (se_other.go), a fake in tests. Errors
// come back as text ("" = success) so the shim's Go side has no branches.
type hardware interface {
	// create makes a fresh, non-permanent enclave key and returns its
	// wrapped blob and its public key as an X9.63 uncompressed point.
	create() (blob, point []byte, errMsg string)
	// sign signs msg (SHA-256, ECDSA, ASN.1 DER) with the key in blob.
	sign(blob, msg []byte) (sig []byte, errMsg string)
}

// Fault-injection seams for the software key's error arms.
var (
	generateKey = ecdsa.GenerateKey
	signASN1    = ecdsa.SignASN1
)

// ErrNoSecureEnclave is signing with an enclave key on a computer (or a
// build) that can't reach one — the file was copied, or the binary was
// built without cgo.
var ErrNoSecureEnclave = errors.New("this ex-runner build can't use the Secure Enclave")

// Key is one device key. It is immutable once made.
type Key struct {
	kind      string
	blob      []byte // enclave: the wrapped key; software: PKCS#8
	publicKey []byte // SPKI DER
	createdAt time.Time
	priv      *ecdsa.PrivateKey // software only
}

// New makes a key in the Secure Enclave when this computer has one that
// works, and a software key otherwise.
func New(now time.Time) (*Key, error) {
	if secureEnclave != nil {
		if k, err := newSecureEnclave(now); err == nil {
			return k, nil
		}
	}
	return NewSoftware(now)
}

func newSecureEnclave(now time.Time) (*Key, error) {
	blob, point, msg := secureEnclave.create()
	if msg != "" {
		return nil, errors.New(msg)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, err
	}
	spki, _ := x509.MarshalPKIXPublicKey(pub) // a valid P-256 key always marshals
	k := &Key{kind: KindSecureEnclave, blob: blob, publicKey: spki, createdAt: now.UTC()}
	// An enclave that makes keys but can't sign with them (seen in VMs)
	// must not become this login's key.
	if err := k.Check(); err != nil {
		return nil, err
	}
	return k, nil
}

// NewSoftware makes a software P-256 key.
func NewSoftware(now time.Time) (*Key, error) {
	priv, err := generateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(priv)         // a P-256 key always marshals
	spki, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey) // likewise
	return &Key{kind: KindSoftware, blob: pkcs8, publicKey: spki, createdAt: now.UTC(), priv: priv}, nil
}

// Kind is KindSecureEnclave or KindSoftware.
func (k *Key) Kind() string { return k.kind }

// Hardware reports whether the private key lives in the Secure Enclave.
func (k *Key) Hardware() bool { return k.kind == KindSecureEnclave }

// PublicKey is the public half as base64 (standard) SPKI DER — what the
// server stores.
func (k *Key) PublicKey() string { return base64.StdEncoding.EncodeToString(k.publicKey) }

// Sign returns an ASN.1 DER ECDSA signature over SHA-256(msg).
func (k *Key) Sign(msg []byte) ([]byte, error) {
	if k.kind == KindSoftware {
		sum := sha256.Sum256(msg)
		return signASN1(rand.Reader, k.priv, sum[:])
	}
	if secureEnclave == nil {
		return nil, ErrNoSecureEnclave
	}
	sig, errMsg := secureEnclave.sign(k.blob, msg)
	if errMsg != "" {
		return nil, errors.New(errMsg)
	}
	return sig, nil
}

// Check signs a probe and verifies it against the public key: the key is
// usable on this computer right now.
func (k *Key) Check() error {
	probe := []byte("ex-runner device key check")
	sig, err := k.Sign(probe)
	if err != nil {
		return err
	}
	pub, _ := x509.ParsePKIXPublicKey(k.publicKey) // validated when made or loaded
	sum := sha256.Sum256(probe)
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig) {
		return errors.New("the device key's signature doesn't match its public key")
	}
	return nil
}

type keyFile struct {
	Kind      string `json:"kind"`
	Blob      string `json:"blob"`
	PublicKey string `json:"publicKey"`
	CreatedAt string `json:"createdAt"`
}

// Save writes the key owner-only (0600) and atomically (write + rename).
func (k *Key) Save(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(keyFile{ // a struct of strings always marshals
		Kind:      k.kind,
		Blob:      base64.StdEncoding.EncodeToString(k.blob),
		PublicKey: k.PublicKey(),
		CreatedAt: k.createdAt.Format(time.RFC3339),
	}, "", "  ")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Load reads a key saved by Save. A missing file is an error that matches
// fs.ErrNotExist; anything malformed is refused rather than half-used.
func Load(file string) (*Key, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var f keyFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(file), err)
	}
	k, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(file), err)
	}
	return k, nil
}

func parse(f keyFile) (*Key, error) {
	if f.Kind != KindSecureEnclave && f.Kind != KindSoftware {
		return nil, fmt.Errorf("unknown key kind %q", f.Kind)
	}
	blob, err := base64.StdEncoding.DecodeString(f.Blob)
	if err != nil || len(blob) == 0 {
		return nil, errors.New("bad key blob")
	}
	spki, err := base64.StdEncoding.DecodeString(f.PublicKey)
	if err != nil {
		return nil, errors.New("bad public key")
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	ec, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("the public key is not a P-256 key")
	}
	created, err := time.Parse(time.RFC3339, f.CreatedAt)
	if err != nil {
		return nil, errors.New("bad createdAt")
	}
	k := &Key{kind: f.Kind, blob: blob, publicKey: spki, createdAt: created}
	if f.Kind == KindSoftware {
		priv, err := x509.ParsePKCS8PrivateKey(blob)
		sk, ok := priv.(*ecdsa.PrivateKey)
		if err != nil || !ok || !sk.PublicKey.Equal(ec) {
			return nil, errors.New("the private key doesn't match the public key")
		}
		k.priv = sk
	}
	return k, nil
}

// Remove deletes a saved key; a missing file is not an error.
func Remove(file string) error {
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Message is what a request proof signs: the request's method and path,
// the time, and the runner token's ID, one per line.
func Message(method, path string, unix int64, tokenID string) []byte {
	return []byte(strings.Join([]string{
		"ex-runner-proof/v1",
		strings.ToUpper(method),
		path,
		strconv.FormatInt(unix, 10),
		tokenID,
	}, "\n"))
}

// Prover signs runner-token requests with key (api.Prover).
type Prover struct {
	key     *Key
	tokenID string
	now     func() time.Time
}

// NewProver signs for the runner token tokenID.
func NewProver(key *Key, tokenID string, now func() time.Time) *Prover {
	return &Prover{key: key, tokenID: tokenID, now: now}
}

// Proof is the X-Ex-Runner-Proof value for one request:
// v1.<unix-seconds>.<base64url DER signature>.
func (p *Prover) Proof(method, path string) (string, error) {
	ts := p.now().Unix()
	sig, err := p.key.Sign(Message(method, path, ts, p.tokenID))
	if err != nil {
		return "", err
	}
	return "v1." + strconv.FormatInt(ts, 10) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
