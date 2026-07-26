package secret

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// Every Worker, Lease, registration and Artifact upload credential in the
// system is minted by NewToken and later checked by comparing a stored digest
// against HashToken of what the caller presented. Those two functions must
// therefore agree exactly: if a change salted or re-encoded one side only,
// either nothing would authenticate or — in the other direction — the stored
// digest would stop being a function of the secret. Neither had any test.
func TestNewTokenAgreesWithHashToken(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, HashToken(plain)) {
		t.Fatal("NewToken digest does not match HashToken of its own plaintext")
	}
	if bytes.Equal(hash, HashToken(plain+"x")) {
		t.Fatal("HashToken ignored a modified secret")
	}
}

func TestNewTokenIsUnpredictableAndFullEntropy(t *testing.T) {
	const rounds = 256
	seenPlain := make(map[string]struct{}, rounds)
	seenHash := make(map[string]struct{}, rounds)
	for round := 0; round < rounds; round++ {
		plain, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		// 32 random bytes, so the raw-URL base64 form is 43 characters and
		// carries no padding that could be trimmed by a caller.
		decoded, err := base64.RawURLEncoding.DecodeString(plain)
		if err != nil {
			t.Fatalf("token is not raw-URL base64: %v", err)
		}
		if len(decoded) != 32 {
			t.Fatalf("token carries %d bytes of entropy, want 32", len(decoded))
		}
		if len(hash) != sha256.Size {
			t.Fatalf("digest is %d bytes, want %d", len(hash), sha256.Size)
		}
		if _, repeated := seenPlain[plain]; repeated {
			t.Fatal("NewToken repeated a secret")
		}
		if _, repeated := seenHash[string(hash)]; repeated {
			t.Fatal("NewToken repeated a digest")
		}
		seenPlain[plain] = struct{}{}
		seenHash[string(hash)] = struct{}{}
	}
}

// The stored digest must not leak the secret it authenticates.
func TestHashTokenIsDeterministicAndHidesThePlaintext(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(HashToken(plain), HashToken(plain)) {
		t.Fatal("HashToken is not deterministic")
	}
	if bytes.Contains(hash, []byte(plain)) {
		t.Fatal("digest embeds the plaintext token")
	}
	if HashToken("") == nil || len(HashToken("")) != sha256.Size {
		t.Fatal("HashToken must still produce a full-width digest for an empty secret")
	}
}
