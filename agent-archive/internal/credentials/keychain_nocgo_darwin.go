//go:build darwin && !cgo

package credentials

import "context"

type KeychainStore struct{}

func NewKeychainStore(service string) (*KeychainStore, error)              { return nil, ErrUnavailable }
func (s *KeychainStore) Save(context.Context, string, R2Credentials) error { return ErrUnavailable }
func (s *KeychainStore) Load(context.Context, string) (R2Credentials, error) {
	return R2Credentials{}, ErrUnavailable
}
func (s *KeychainStore) Delete(context.Context, string) error { return ErrUnavailable }
