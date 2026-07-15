package util

import (
	"crypto/rand"
	"fmt"
	"io"
	"strconv"
)

const (
	TargetKeyLength         = 32
	MinRandomKeyLength      = 25
	MinRandomKeyEntropyBits = 128
	MaxPrefixLength         = TargetKeyLength - MinRandomKeyLength

	base36Chars          = "0123456789abcdefghijklmnopqrstuvwxyz"
	base36RejectionLimit = 252 // largest multiple of 36 below 256
)

// KeyGenerator generates fixed-length redemption keys. It intentionally has
// no process-local state: uniqueness comes from cryptographically secure
// randomness and is enforced by the database's unique index.
type KeyGenerator struct{}

var defaultKeyGen = &KeyGenerator{}

// GenerateKey creates a single 32-character redemption key.
func GenerateKey(prefix string) (string, error) {
	return defaultKeyGen.Generate(prefix)
}

// GenerateBatch creates a batch of unique redemption keys.
func GenerateBatch(count int, prefix string) ([]string, error) {
	return defaultKeyGen.GenerateBatch(count, prefix)
}

// Generate creates a single 32-character redemption key. The random suffix
// uses lowercase base36 so case-insensitive database collations preserve all
// of its entropy. With the longest allowed prefix, 25 random symbols provide
// 25*log2(36) ~= 129.25 bits of entropy.
func (g *KeyGenerator) Generate(prefix string) (string, error) {
	if err := validateKeyPrefix(prefix); err != nil {
		return "", err
	}

	randomPart, err := randomBase36(TargetKeyLength - len(prefix))
	if err != nil {
		return "", err
	}

	return prefix + randomPart, nil
}

// GenerateBatch creates a batch of unique 32-character redemption keys.
func (g *KeyGenerator) GenerateBatch(count int, prefix string) ([]string, error) {
	if count < 1 || count > 1000 {
		return nil, fmt.Errorf("count must be between 1 and 1000")
	}
	if err := validateKeyPrefix(prefix); err != nil {
		return nil, err
	}

	keySet := make(map[string]struct{}, count)
	keys := make([]string, 0, count)
	maxAttempts := count * 3

	for len(keys) < count && maxAttempts > 0 {
		key, err := g.Generate(prefix)
		if err != nil {
			return nil, err
		}
		if _, exists := keySet[key]; !exists {
			keySet[key] = struct{}{}
			keys = append(keys, key)
		}
		maxAttempts--
	}

	if len(keys) < count {
		return nil, fmt.Errorf("failed to generate %d unique keys", count)
	}

	return keys, nil
}

func validateKeyPrefix(prefix string) error {
	if len(prefix) > MaxPrefixLength {
		return fmt.Errorf("prefix must not exceed %d characters", MaxPrefixLength)
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return fmt.Errorf("prefix may only contain lowercase letters, digits, '_' and '-'")
	}
	return nil
}

func randomBase36(length int) (string, error) {
	return randomBase36From(rand.Reader, length)
}

func randomBase36From(reader io.Reader, length int) (string, error) {
	if length < 0 {
		return "", fmt.Errorf("random length must not be negative")
	}
	if length == 0 {
		return "", nil
	}

	result := make([]byte, 0, length)
	bufferSize := length * 2
	if bufferSize < 32 {
		bufferSize = 32
	}
	randomBytes := make([]byte, bufferSize)

	for len(result) < length {
		if _, err := io.ReadFull(reader, randomBytes); err != nil {
			return "", fmt.Errorf("failed to generate random bytes: %w", err)
		}
		for _, value := range randomBytes {
			if value >= base36RejectionLimit {
				continue
			}
			result = append(result, base36Chars[int(value)%len(base36Chars)])
			if len(result) == length {
				break
			}
		}
	}

	return string(result), nil
}

// Base36Encode encodes an integer to a lowercase base36 string.
func Base36Encode(n int64) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + Base36Encode(-n)
	}
	return strconv.FormatInt(n, 36)
}
