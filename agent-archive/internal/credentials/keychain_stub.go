//go:build !darwin

package credentials

import "context"

// KeychainStore is unavailable off macOS. Keeping the stub lets package tests
// and non-macOS builds fail with an actionable error rather than shelling out
// to a platform command.
type KeychainStore struct{}

func NewKeychainStore(service string) (*KeychainStore, error)              { return nil, ErrUnavailable }
func (s *KeychainStore) Save(context.Context, string, R2Credentials) error { return ErrUnavailable }
func (s *KeychainStore) Load(context.Context, string) (R2Credentials, error) {
	return R2Credentials{}, ErrUnavailable
}
func (s *KeychainStore) Delete(context.Context, string) error { return ErrUnavailable }
