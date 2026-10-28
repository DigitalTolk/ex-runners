package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/devicekey"
	"github.com/DigitalTolk/ex-runners/internal/runner"
)

// verifyProof checks an X-Ex-Runner-Proof header against key.
func verifyProof(key *devicekey.Key, method, path, tokenID, proof string) bool {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	ts, _ := strconv.ParseInt(parts[1], 10, 64)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	spki, _ := base64.StdEncoding.DecodeString(key.PublicKey())
	pub, _ := x509.ParsePKIXPublicKey(spki)
	sum := sha256.Sum256(devicekey.Message(method, path, ts, tokenID))
	return ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], sig)
}

// enclaveKeyFile writes a Secure Enclave key file whose blob no enclave
// wrapped: it loads (and reports "hardware") but never signs.
func enclaveKeyFile(t *testing.T, file string) *devicekey.Key {
	t.Helper()
	sw, _ := devicekey.NewSoftware(t0)
	raw, _ := json.Marshal(map[string]string{
		"kind": devicekey.KindSecureEnclave, "blob": base64.StdEncoding.EncodeToString([]byte("wrapped")),
		"publicKey": sw.PublicKey(), "createdAt": "2026-10-05T00:00:00Z",
	})
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := devicekey.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) request(path string) (body map[string]any, proof string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.requests {
		if strings.Split(r, " ")[1] == path {
			body, proof, ok = e.bodies[i], e.proofs[i], true
		}
	}
	return body, proof, ok
}

func TestLoginRegistersAFreshDeviceKey(t *testing.T) {
	e := newEnv(t)
	open := e.approve()
	code := Run([]string{"login", e.srv.URL}, e.deps(func(d *Deps) { d.OpenBrowser = open }))
	if code != 0 {
		t.Fatal(e.errText())
	}
	first, err := devicekey.Load(deviceKeyPath(e.home))
	if err != nil || first.Hardware() {
		t.Fatalf("key = %+v, %v", first, err)
	}
	body, _, _ := e.request("/api/v1/runner-tokens/exchange")
	dk, _ := body["deviceKey"].(map[string]any)
	if dk["publicKey"] != first.PublicKey() || dk["hardware"] != false || body["code"] != "c" {
		t.Errorf("exchange body = %v", body)
	}
	if c, _ := loadCredentials(credentialsPath(e.home)); !c.DeviceKey {
		t.Errorf("credentials not marked as locked: %+v", c)
	}
	if !strings.Contains(e.outText(), "locked to a key on this computer") {
		t.Errorf("out = %s", e.outText())
	}

	// Re-login: a new key replaces the old one, and the old token is
	// retired with a proof from the OLD key (anything else reads as theft).
	e.out = nil
	newDeviceKey = func(now time.Time) (*devicekey.Key, error) {
		return enclaveKeyFile(t, filepath.Join(t.TempDir(), "k.json")), nil
	}
	defer func() { newDeviceKey = devicekey.NewSoftware }()
	if Run([]string{"login", e.srv.URL}, e.deps(func(d *Deps) { d.OpenBrowser = e.approve() })) != 0 {
		t.Fatal(e.errText())
	}
	second, _ := devicekey.Load(deviceKeyPath(e.home))
	if second.PublicKey() == first.PublicKey() || !second.Hardware() {
		t.Error("re-login must replace the device key")
	}
	_, proof, ok := e.request("/api/v1/agent/runner/revoke")
	if !ok || !verifyProof(first, "POST", "/api/v1/agent/runner/revoke", "rt2", proof) {
		t.Errorf("old token's revoke proof = %q", proof)
	}
	if !strings.Contains(e.outText(), "locked to this Mac's Secure Enclave") {
		t.Errorf("out = %s", e.outText())
	}
}

func TestStartRefusesAMissingOrBrokenDeviceKey(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	_ = os.Remove(deviceKeyPath(e.home))
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "can't use (device-key.json is missing)") || !strings.Contains(e.errText(), "ex-runner login "+e.srv.URL) {
		t.Errorf("missing key = %s", e.errText())
	}
	e.err = nil
	enclaveKeyFile(t, deviceKeyPath(e.home)) // copied from another Mac
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "can't use (") || strings.Contains(e.errText(), "missing") {
		t.Errorf("unusable key = %s", e.errText())
	}
	e.err = nil
	_ = os.WriteFile(deviceKeyPath(e.home), []byte("{broken"), 0o600)
	if Run([]string{"start"}, e.deps(nil)) != 1 || !strings.Contains(e.errText(), "read device-key.json") {
		t.Errorf("broken key = %s", e.errText())
	}
	if len(e.reqs()) != 0 {
		t.Errorf("nothing may reach the server: %v", e.reqs())
	}
}

func TestStartSignsEverythingWithTheDeviceKey(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(func(c *Credentials) { c.ExpiresAt = t0.Add(2 * 24 * time.Hour).Format(time.RFC3339Nano) })
	key, _ := devicekey.Load(deviceKeyPath(e.home))
	e.answer("/api/v1/agent/runner/renew", "200", `{"token":"renewed","expiresAt":"2026-10-12T00:00:00Z"}`)
	var cfg runner.Config
	go func() {
		waitFor(t, func() bool { return strings.Contains(e.outText(), "ex-runner is online") })
		close(e.shutdown)
	}()
	if Run([]string{"start"}, e.deps(func(d *Deps) {
		d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) { cfg = c; return &fakeHandle{}, nil }
	})) != 0 {
		t.Fatal(e.errText())
	}
	body, proof, _ := e.request("/api/v1/agent/runner/renew")
	if len(body) != 0 || !verifyProof(key, "POST", "/api/v1/agent/runner/renew", "rt1", proof) {
		t.Errorf("renew body=%v proof=%q", body, proof)
	}
	// The runner gets the same prover.
	p, err := cfg.Prover.Proof("POST", "/api/v1/agent/runner/heartbeat")
	if err != nil || !verifyProof(key, "POST", "/api/v1/agent/runner/heartbeat", "rt1", p) {
		t.Errorf("runner proof = %q, %v", p, err)
	}
	if strings.Contains(e.outText(), "isn't locked") {
		t.Error("a locked sign-in needs no hint")
	}
}

func runUntilOnline(t *testing.T, e *env, over func(d *Deps)) int {
	t.Helper()
	go func() {
		waitFor(t, func() bool { return strings.Contains(e.outText(), "ex-runner is online") })
		close(e.shutdown)
	}()
	return Run([]string{"start"}, e.deps(over))
}

func TestStartKeepsAKeylessSignInRunning(t *testing.T) {
	// A sign-in from before device keys runs unsigned, with a hint; a stray
	// key file doesn't change that.
	e := newEnv(t)
	e.saveCreds(func(c *Credentials) {
		c.DeviceKey = false
		c.ExpiresAt = t0.Add(2 * 24 * time.Hour).Format(time.RFC3339Nano)
	})
	e.saveKey()
	e.answer("/api/v1/agent/runner/renew", "200", `{"token":"renewed","expiresAt":"2026-10-12T00:00:00Z"}`)
	var cfg runner.Config
	if runUntilOnline(t, e, func(d *Deps) {
		d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) { cfg = c; return &fakeHandle{}, nil }
	}) != 0 {
		t.Fatal(e.errText())
	}
	if !strings.Contains(e.outText(), "isn't locked to this computer. To lock it, run: ex-runner login "+e.srv.URL) {
		t.Errorf("out = %s", e.outText())
	}
	if body, proof, ok := e.request("/api/v1/agent/runner/renew"); !ok || len(body) != 0 || proof != "" || cfg.Prover != nil {
		t.Errorf("renew body=%v proof=%q prover=%v", body, proof, cfg.Prover)
	}
	if c, _ := loadCredentials(credentialsPath(e.home)); c.DeviceKey || c.Token != "renewed" {
		t.Errorf("credentials = %+v", c)
	}

	// The server wants locked sign-ins and turns the renewal down.
	e2 := newEnv(t)
	e2.saveCreds(func(c *Credentials) {
		c.DeviceKey = false
		c.ExpiresAt = t0.Add(2 * 24 * time.Hour).Format(time.RFC3339Nano)
	})
	e2.answer("/api/v1/agent/runner/renew", "401", `{"error":{"code":"runner_upgrade_required","message":"update"}}`)
	if Run([]string{"start"}, e2.deps(nil)) != 1 || !strings.Contains(e2.errText(), "Update ex-runner to the latest version") {
		t.Errorf("upgrade required at renew = %s", e2.errText())
	}
}

func TestStartExplainsClockSkew(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	skewed := true
	if runUntilOnline(t, e, func(d *Deps) {
		d.StartRunner = func(context.Context, runner.Config) (runner.Handle, error) {
			if skewed {
				skewed = false
				return nil, &api.Error{Status: 401, Code: api.CodeClockSkew, Message: "clock skew"}
			}
			return &fakeHandle{}, nil
		}
	}) != 0 || !strings.Contains(e.errText(), "more than 10 minutes off — set the correct time; retrying in ") {
		t.Errorf("err = %s", e.errText())
	}
}

func TestUpgradeRequiredMessages(t *testing.T) {
	upgrade := &api.Error{Status: 401, Code: api.CodeUpgradeRequired, Message: "update"}
	e := newEnv(t)
	e.saveCreds(nil)
	if Run([]string{"start"}, e.deps(func(d *Deps) {
		d.StartRunner = func(context.Context, runner.Config) (runner.Handle, error) { return nil, upgrade }
	})) != 1 || !strings.Contains(e.errText(), "locked to the computer they run on") {
		t.Errorf("at register = %s", e.errText())
	}
	e2 := newEnv(t)
	e2.saveCreds(nil)
	if Run([]string{"start"}, e2.deps(func(d *Deps) {
		d.StartRunner = func(_ context.Context, c runner.Config) (runner.Handle, error) {
			go c.OnUnauthorized(upgrade)
			return &fakeHandle{}, nil
		}
	})) != 1 || !strings.Contains(e2.errText(), "Update ex-runner to the latest version, then connect it again with: ex-runner login "+e2.srv.URL) {
		t.Errorf("while running = %s", e2.errText())
	}
}

func TestStatusReportsTheLock(t *testing.T) {
	e := newEnv(t)
	status := func() (string, statusJSON) {
		t.Helper()
		e.out = nil
		Run([]string{"status"}, e.deps(nil))
		text := e.outText()
		e.out = nil
		Run([]string{"status", "--json"}, e.deps(nil))
		var st statusJSON
		_ = json.Unmarshal([]byte(e.outText()), &st)
		return text, st
	}
	e.saveCreds(nil)
	key, _ := devicekey.Load(deviceKeyPath(e.home))
	text, st := status()
	if !strings.Contains(text, "Locked:   a key on this computer") || st.DeviceKey != "software" {
		t.Errorf("software: %s / %+v", text, st)
	}
	if strings.Contains(e.outText(), key.PublicKey()) {
		t.Error("status must never print the key")
	}
	enclaveKeyFile(t, deviceKeyPath(e.home))
	if text, st := status(); !strings.Contains(text, "Locked:   this Mac's Secure Enclave") || st.DeviceKey != "hardware" {
		t.Errorf("hardware: %s / %+v", text, st)
	}
	_ = os.Remove(deviceKeyPath(e.home))
	if text, st := status(); !strings.Contains(text, "missing here — run: ex-runner login") || st.DeviceKey != "" {
		t.Errorf("missing: %s / %+v", text, st)
	}
	e.saveCreds(func(c *Credentials) { c.DeviceKey = false })
	if text, st := status(); !strings.Contains(text, "Locked:   no — to lock it to this computer, run: ex-runner login") || st.DeviceKey != "" || !strings.Contains(e.outText(), `"deviceKey":""`) {
		t.Errorf("keyless: %s / %+v", text, st)
	}
}

func TestLogoutDeletesTheDeviceKey(t *testing.T) {
	e := newEnv(t)
	e.saveCreds(nil)
	key, _ := devicekey.Load(deviceKeyPath(e.home))
	if Run([]string{"logout"}, e.deps(nil)) != 0 {
		t.Fatal(e.errText())
	}
	if _, proof, _ := e.request("/api/v1/agent/runner/revoke"); !verifyProof(key, "POST", "/api/v1/agent/runner/revoke", "rt1", proof) {
		t.Errorf("revoke proof = %q", proof)
	}
	if _, err := os.Stat(deviceKeyPath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Error("device key not deleted")
	}
	// Not connected: a leftover key goes too.
	e.saveKey()
	if Run([]string{"logout"}, e.deps(nil)) != 0 {
		t.Fatal(e.errText())
	}
	if _, err := os.Stat(deviceKeyPath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Error("leftover device key not deleted")
	}
}
