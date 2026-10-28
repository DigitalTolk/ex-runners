//go:build !darwin || !cgo

package devicekey

// No Secure Enclave outside macOS, or in a build without cgo: every key is
// a software key (see se_darwin.go for the enclave).
var secureEnclave hardware
