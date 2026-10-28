//go:build darwin && cgo

package devicekey

// The Secure Enclave shim. The C below only moves bytes between Go and
// Security.framework; every decision lives in devicekey.go. Its Go side has
// no branches (errors come back as text), so one call of each function
// covers it whether or not this Mac has a working enclave.
//
// The key is created with kSecAttrIsPermanent=false: it is never added to
// the Keychain, so there is no prompt, no access group and no entitlement.
// What persists is the key's "toid" attribute — a blob wrapped by this
// Mac's enclave, useless anywhere else — from which SecKeyCreateWithData
// rebuilds the key for each signature.

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

typedef struct { uint8_t *data; size_t len; } exBuf;

static void exPutBytes(exBuf *out, const void *p, size_t n) {
	out->data = malloc(n ? n : 1);
	out->len = n;
	memcpy(out->data, p, n);
}

static void exPutData(exBuf *out, CFDataRef d) {
	exPutBytes(out, CFDataGetBytePtr(d), (size_t)CFDataGetLength(d));
}

static void exPutError(exBuf *out, const char *what, CFErrorRef err) {
	char msg[512];
	char desc[400] = "unknown error";
	if (err) {
		CFStringRef s = CFErrorCopyDescription(err);
		if (s) {
			CFStringGetCString(s, desc, sizeof desc, kCFStringEncodingUTF8);
			CFRelease(s);
		}
		CFRelease(err);
	}
	snprintf(msg, sizeof msg, "secure enclave: %s: %s", what, desc);
	exPutBytes(out, msg, strlen(msg));
}

static CFMutableDictionaryRef exDict(void) {
	return CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
}

static void exSECreate(exBuf *blob, exBuf *point, exBuf *errOut) {
	CFErrorRef err = NULL;
	int bits = 256;
	CFNumberRef size = CFNumberCreate(NULL, kCFNumberIntType, &bits);
	CFMutableDictionaryRef priv = exDict();
	CFDictionarySetValue(priv, kSecAttrIsPermanent, kCFBooleanFalse);
	CFMutableDictionaryRef attrs = exDict();
	CFDictionarySetValue(attrs, kSecAttrKeyType, kSecAttrKeyTypeECSECPrimeRandom);
	CFDictionarySetValue(attrs, kSecAttrKeySizeInBits, size);
	CFDictionarySetValue(attrs, kSecAttrTokenID, kSecAttrTokenIDSecureEnclave);
	CFDictionarySetValue(attrs, kSecPrivateKeyAttrs, priv);
	SecKeyRef key = SecKeyCreateRandomKey(attrs, &err);
	CFRelease(attrs);
	CFRelease(priv);
	CFRelease(size);
	if (!key) {
		exPutError(errOut, "create key", err);
		return;
	}
	CFDictionaryRef ka = SecKeyCopyAttributes(key);
	CFDataRef toid = ka ? CFDictionaryGetValue(ka, CFSTR("toid")) : NULL;
	SecKeyRef pub = SecKeyCopyPublicKey(key);
	CFDataRef ext = pub ? SecKeyCopyExternalRepresentation(pub, &err) : NULL;
	if (toid && CFGetTypeID(toid) == CFDataGetTypeID() && ext) {
		exPutData(blob, toid);
		exPutData(point, ext);
	} else {
		exPutError(errOut, "export key", err);
	}
	if (ext) CFRelease(ext);
	if (pub) CFRelease(pub);
	if (ka) CFRelease(ka);
	CFRelease(key);
}

static void exSESign(const uint8_t *blob, size_t blobLen, const uint8_t *msg, size_t msgLen, exBuf *sig, exBuf *errOut) {
	CFErrorRef err = NULL;
	CFDataRef toid = CFDataCreate(NULL, blob, (CFIndex)blobLen);
	CFMutableDictionaryRef attrs = exDict();
	CFDictionarySetValue(attrs, kSecAttrKeyType, kSecAttrKeyTypeECSECPrimeRandom);
	CFDictionarySetValue(attrs, kSecAttrKeyClass, kSecAttrKeyClassPrivate);
	CFDictionarySetValue(attrs, kSecAttrTokenID, kSecAttrTokenIDSecureEnclave);
	CFDictionarySetValue(attrs, CFSTR("toid"), toid);
	SecKeyRef key = SecKeyCreateWithData(toid, attrs, &err);
	CFRelease(attrs);
	CFRelease(toid);
	if (!key) {
		exPutError(errOut, "load key", err);
		return;
	}
	CFDataRef data = CFDataCreate(NULL, msg, (CFIndex)msgLen);
	CFDataRef s = SecKeyCreateSignature(key, kSecKeyAlgorithmECDSASignatureMessageX962SHA256, data, &err);
	CFRelease(data);
	CFRelease(key);
	if (!s) {
		exPutError(errOut, "sign", err);
		return;
	}
	exPutData(sig, s);
	CFRelease(s);
}
*/
import "C"

import "unsafe"

var secureEnclave hardware = seBackend{}

type seBackend struct{}

// take copies a C buffer into Go and frees it (free(NULL) is a no-op, and
// GoBytes of a NULL, 0-length buffer is empty).
func take(b C.exBuf) []byte {
	defer C.free(unsafe.Pointer(b.data))
	return C.GoBytes(unsafe.Pointer(b.data), C.int(b.len))
}

func (seBackend) create() (blob, point []byte, errMsg string) {
	var b, p, e C.exBuf
	C.exSECreate(&b, &p, &e)
	return take(b), take(p), string(take(e))
}

func (seBackend) sign(blob, msg []byte) (sig []byte, errMsg string) {
	cb, cm := C.CBytes(blob), C.CBytes(msg)
	defer C.free(cb)
	defer C.free(cm)
	var s, e C.exBuf
	C.exSESign((*C.uint8_t)(cb), C.size_t(len(blob)), (*C.uint8_t)(cm), C.size_t(len(msg)), &s, &e)
	return take(s), string(take(e))
}
