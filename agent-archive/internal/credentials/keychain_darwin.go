//go:build darwin && cgo

package credentials

/*
#cgo darwin LDFLAGS: -framework Security -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

static CFStringRef aa_string(const char *value) {
	return CFStringCreateWithCString(NULL, value, kCFStringEncodingUTF8);
}

static int aa_keychain_get(const char *service, const char *account, void **out, size_t *out_len) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecReturnData, kSecUseAuthenticationUI };
	const void *values[] = { kSecClassGenericPassword, svc, acct, kCFBooleanTrue, kSecUseAuthenticationUIFail };
	CFDictionaryRef query = CFDictionaryCreate(NULL, keys, values, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef result = NULL;
	OSStatus status = SecItemCopyMatching(query, &result);
	CFRelease(query); CFRelease(svc); CFRelease(acct);
	if (status != errSecSuccess) return (int)status;
	CFDataRef data = (CFDataRef)result;
	CFIndex len = CFDataGetLength(data);
	void *copy = malloc((size_t)len);
	if (copy == NULL) { CFRelease(result); return (int)errSecAllocate; }
	memcpy(copy, CFDataGetBytePtr(data), (size_t)len);
	*out = copy; *out_len = (size_t)len;
	CFRelease(result);
	return 0;
}

static int aa_keychain_save(const char *service, const char *account, const void *bytes, size_t len) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	CFDataRef data = CFDataCreate(NULL, bytes, (CFIndex)len);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecValueData };
	const void *values[] = { kSecClassGenericPassword, svc, acct, data };
	CFDictionaryRef item = CFDictionaryCreate(NULL, keys, values, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemAdd(item, NULL);
	CFRelease(item);
	if (status != errSecDuplicateItem) { CFRelease(data); CFRelease(svc); CFRelease(acct); return (int)status; }
	const void *queryKeys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
	const void *queryValues[] = { kSecClassGenericPassword, svc, acct };
	CFDictionaryRef query = CFDictionaryCreate(NULL, queryKeys, queryValues, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *changeKeys[] = { kSecValueData };
	const void *changeValues[] = { data };
	CFDictionaryRef changes = CFDictionaryCreate(NULL, changeKeys, changeValues, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	status = SecItemUpdate(query, changes);
	CFRelease(changes); CFRelease(query); CFRelease(data); CFRelease(svc); CFRelease(acct);
	return (int)status;
}

static int aa_keychain_delete(const char *service, const char *account) {
	CFStringRef svc = aa_string(service), acct = aa_string(account);
	const void *keys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
	const void *values[] = { kSecClassGenericPassword, svc, acct };
	CFDictionaryRef query = CFDictionaryCreate(NULL, keys, values, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	OSStatus status = SecItemDelete(query);
	CFRelease(query); CFRelease(svc); CFRelease(acct);
	return status == errSecItemNotFound ? 0 : (int)status;
}

static void aa_keychain_free(void *value) { free(value); }
*/
import "C"

import (
	"context"
	"errors"
	"runtime"
	"unsafe"
)

// KeychainStore uses Security.framework directly. It never invokes the
// `security` command, which would expose a secret through argv or shell logs.
type KeychainStore struct{ service string }

func NewKeychainStore(service string) (*KeychainStore, error) {
	if service == "" {
		return nil, errors.New("keychain service is required")
	}
	return &KeychainStore{service: service}, nil
}

func (s *KeychainStore) Save(ctx context.Context, reference string, value R2Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reference == "" {
		return ErrInvalidReference
	}
	encoded, err := EncodeSecret(value)
	if err != nil {
		return err
	}
	cService, cReference := C.CString(s.service), C.CString(reference)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cReference))
	status := C.aa_keychain_save(cService, cReference, unsafe.Pointer(&encoded[0]), C.size_t(len(encoded)))
	runtime.KeepAlive(encoded)
	if status != 0 {
		return ErrUnavailable
	}
	return nil
}

func (s *KeychainStore) Load(ctx context.Context, reference string) (R2Credentials, error) {
	if err := ctx.Err(); err != nil {
		return R2Credentials{}, err
	}
	if reference == "" {
		return R2Credentials{}, ErrInvalidReference
	}
	cService, cReference := C.CString(s.service), C.CString(reference)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cReference))
	var data unsafe.Pointer
	var length C.size_t
	status := C.aa_keychain_get(cService, cReference, &data, &length)
	if status != 0 {
		return R2Credentials{}, ErrUnavailable
	}
	defer C.aa_keychain_free(data)
	return DecodeSecret(C.GoBytes(data, C.int(length)))
}

func (s *KeychainStore) Delete(ctx context.Context, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reference == "" {
		return ErrInvalidReference
	}
	cService, cReference := C.CString(s.service), C.CString(reference)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cReference))
	if status := C.aa_keychain_delete(cService, cReference); status != 0 {
		return ErrUnavailable
	}
	return nil
}

var _ CredentialStore = (*KeychainStore)(nil)
