package util

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestKeyEntropyBoundary(t *testing.T) {
	if TargetKeyLength != 32 {
		t.Fatalf("TargetKeyLength = %d, want 32", TargetKeyLength)
	}
	if MaxPrefixLength != 7 {
		t.Fatalf("MaxPrefixLength = %d, want 7", MaxPrefixLength)
	}
	if TargetKeyLength-MaxPrefixLength != MinRandomKeyLength {
		t.Fatalf("minimum random suffix length = %d, want %d", TargetKeyLength-MaxPrefixLength, MinRandomKeyLength)
	}

	bitsPerCharacter := math.Log2(float64(len(base36Chars)))
	minimumEntropy := float64(MinRandomKeyLength) * bitsPerCharacter
	if minimumEntropy < MinRandomKeyEntropyBits {
		t.Fatalf("minimum entropy = %.6f bits, want at least %d", minimumEntropy, MinRandomKeyEntropyBits)
	}
	if float64(MinRandomKeyLength-1)*bitsPerCharacter >= MinRandomKeyEntropyBits {
		t.Fatalf("%d base36 symbols unexpectedly meet the %d-bit boundary", MinRandomKeyLength-1, MinRandomKeyEntropyBits)
	}
	if base36RejectionLimit != 252 || base36RejectionLimit%len(base36Chars) != 0 {
		t.Fatalf("rejection limit = %d, want 252 divisible by alphabet size %d", base36RejectionLimit, len(base36Chars))
	}
}

func TestGenerateKeyLengthPrefixAndAlphabet(t *testing.T) {
	prefixes := []string{"", "a", "vip-123", "az09_-x"}
	for _, prefix := range prefixes {
		t.Run(prefix, func(t *testing.T) {
			key, err := GenerateKey(prefix)
			if err != nil {
				t.Fatalf("GenerateKey(%q) returned error: %v", prefix, err)
			}
			if len(key) != TargetKeyLength {
				t.Fatalf("key length = %d, want %d: %q", len(key), TargetKeyLength, key)
			}
			if !strings.HasPrefix(key, prefix) {
				t.Fatalf("key %q does not preserve prefix %q", key, prefix)
			}

			randomPart := key[len(prefix):]
			if len(randomPart) < MinRandomKeyLength {
				t.Fatalf("random suffix length = %d, want at least %d", len(randomPart), MinRandomKeyLength)
			}
			for i := 0; i < len(randomPart); i++ {
				if !strings.ContainsRune(base36Chars, rune(randomPart[i])) {
					t.Fatalf("random suffix contains non-base36 character %q in %q", randomPart[i], randomPart)
				}
			}
		})
	}
}

func TestGenerateKeyRejectsInvalidPrefixes(t *testing.T) {
	invalidPrefixes := []string{
		"abcdefgh",
		"VIP",
		"bad prefix",
		"abc.def",
		"abc/def",
		"中文",
		"abc\n",
	}
	for _, prefix := range invalidPrefixes {
		t.Run(prefix, func(t *testing.T) {
			if _, err := GenerateKey(prefix); err == nil {
				t.Fatalf("GenerateKey(%q) succeeded, want validation error", prefix)
			}
		})
	}
}

func TestGenerateBatchThousandKeysRemainUniqueAfterCaseFold(t *testing.T) {
	const prefix = "vip-123"
	keys, err := GenerateBatch(1000, prefix)
	if err != nil {
		t.Fatalf("GenerateBatch returned error: %v", err)
	}
	if len(keys) != 1000 {
		t.Fatalf("generated %d keys, want 1000", len(keys))
	}

	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if len(key) != TargetKeyLength || !strings.HasPrefix(key, prefix) {
			t.Fatalf("invalid generated key %q", key)
		}
		folded := strings.ToLower(key)
		if _, exists := seen[folded]; exists {
			t.Fatalf("duplicate key after case folding: %q", key)
		}
		seen[folded] = struct{}{}
	}
}

func TestGenerateBatchValidatesCountAndPrefix(t *testing.T) {
	for _, count := range []int{0, 1001} {
		if _, err := GenerateBatch(count, "valid"); err == nil {
			t.Fatalf("GenerateBatch(%d, valid) succeeded, want count error", count)
		}
	}
	if _, err := GenerateBatch(1, "INVALID"); err == nil {
		t.Fatal("GenerateBatch accepted an uppercase prefix")
	}
}

func TestRandomBase36UsesRejectionSampling(t *testing.T) {
	input := []byte{252, 253, 254, 255, 0, 35, 36, 71}
	input = append(input, bytes.Repeat([]byte{0}, 32-len(input))...)

	got, err := randomBase36From(bytes.NewReader(input), 4)
	if err != nil {
		t.Fatalf("randomBase36From returned error: %v", err)
	}
	if got != "0z0z" {
		t.Fatalf("randomBase36From = %q, want %q", got, "0z0z")
	}
}
