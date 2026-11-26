package desiredstate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// keySize is the size of an AES-256 key.
const keySize = 32

var keyIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Sealer encrypts Secret values before they leave the process. Every value is
// bound to its object and data key, so ciphertexts cannot be swapped.
type Sealer struct {
	activeID string
	keys     map[string]cipher.AEAD
	macKey   []byte
}

// ParseKeys decodes "<id>=<base64 key>" pairs separated by commas. Errors
// never contain key material.
func ParseKeys(encoded string) (map[string][]byte, error) {
	keys := map[string][]byte{}
	for pair := range strings.SplitSeq(encoded, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, value, ok := strings.Cut(pair, "=")
		if !ok || !keyIDPattern.MatchString(id) {
			return nil, errors.New("each encryption key must be <id>=<base64 key>, with an id of lowercase letters, digits, '-' or '_'")
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != keySize {
			return nil, fmt.Errorf("encryption key %q must be %d bytes of standard base64", id, keySize)
		}
		if _, exists := keys[id]; exists {
			return nil, fmt.Errorf("encryption key %q is defined twice", id)
		}
		keys[id] = key
	}
	return keys, nil
}

// NewSealer returns a Sealer that encrypts with the active key and decrypts
// with any of keys.
func NewSealer(activeID string, keys map[string][]byte) (*Sealer, error) {
	active, ok := keys[activeID]
	if !ok {
		return nil, fmt.Errorf("active encryption key %q is not defined", activeID)
	}
	s := &Sealer{activeID: activeID, keys: make(map[string]cipher.AEAD, len(keys))}
	for id, key := range keys {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("encryption key %q: %w", id, err)
		}
		if s.keys[id], err = cipher.NewGCM(block); err != nil {
			return nil, fmt.Errorf("encryption key %q: %w", id, err)
		}
	}
	derived := hmac.New(sha256.New, active)
	derived.Write([]byte("capp-backend live version"))
	s.macKey = derived.Sum(nil)
	return s, nil
}

// Seal encrypts the values of a Secret, ordered by key.
func (s *Sealer) Seal(ref Ref, data map[string][]byte) ([]SealedValue, error) {
	if len(data) == 0 {
		return nil, nil
	}
	aead := s.keys[s.activeID]
	sealed := make([]SealedValue, 0, len(data))
	for _, key := range slices.Sorted(maps.Keys(data)) {
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return nil, fmt.Errorf("generating nonce: %w", err)
		}
		sealed = append(sealed, SealedValue{
			Key:        key,
			KeyID:      s.activeID,
			Nonce:      nonce,
			Ciphertext: aead.Seal(nil, nonce, data[key], additionalData(ref, key)),
		})
	}
	return sealed, nil
}

// Open decrypts the values of a Secret.
func (s *Sealer) Open(ref Ref, sealed []SealedValue) (map[string][]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	data := make(map[string][]byte, len(sealed))
	for _, value := range sealed {
		aead, ok := s.keys[value.KeyID]
		if !ok {
			return nil, fmt.Errorf("decrypting %s: encryption key %q is not defined", ref.ID(), value.KeyID)
		}
		if len(value.Nonce) != aead.NonceSize() {
			return nil, fmt.Errorf("decrypting %s: invalid nonce", ref.ID())
		}
		plain, err := aead.Open(nil, value.Nonce, value.Ciphertext, additionalData(ref, value.Key))
		if err != nil {
			return nil, fmt.Errorf("decrypting %s: value does not belong to this object or key", ref.ID())
		}
		if plain == nil {
			plain = []byte{}
		}
		data[value.Key] = plain
	}
	return data, nil
}

// version returns an opaque token for a state. It is keyed, so the token of a
// Secret reveals nothing about its values.
func (s *Sealer) version(payload []byte) string {
	mac := hmac.New(sha256.New, s.macKey)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// additionalData cannot be ambiguous: neither part can contain a NUL byte.
func additionalData(ref Ref, key string) []byte {
	return []byte(ref.ID() + "\x00" + key)
}
