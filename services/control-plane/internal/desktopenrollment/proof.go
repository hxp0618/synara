package desktopenrollment

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const redemptionProofDomain = "synara.desktop-enrollment.redeem.v1"
const rotationProofDomain = "synara.desktop-session.rotate.v1"

type verifiedRedemptionProof struct {
	PublicKey       ed25519.PublicKey
	PublicKeySHA256 []byte
	Nonce           []byte
	NonceSHA256     []byte
}

func verifyRedemptionProof(input RedeemInput) (verifiedRedemptionProof, bool) {
	publicKey, err := base64.RawURLEncoding.DecodeString(input.DevicePublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return verifiedRedemptionProof{}, false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(input.Nonce)
	if err != nil || len(nonce) != 32 {
		return verifiedRedemptionProof{}, false
	}
	proof, err := base64.RawURLEncoding.DecodeString(input.Proof)
	if err != nil || len(proof) != ed25519.SignatureSize {
		return verifiedRedemptionProof{}, false
	}
	message := strings.Join([]string{
		redemptionProofDomain,
		input.ControlPlaneOrigin,
		input.Enrollment,
		input.DevicePublicKey,
		input.Nonce,
	}, "\n")
	if !ed25519.Verify(ed25519.PublicKey(publicKey), []byte(message), proof) {
		return verifiedRedemptionProof{}, false
	}
	keyHash := sha256.Sum256(publicKey)
	nonceHash := sha256.Sum256(nonce)
	return verifiedRedemptionProof{
		PublicKey:       ed25519.PublicKey(publicKey),
		PublicKeySHA256: keyHash[:],
		Nonce:           nonce,
		NonceSHA256:     nonceHash[:],
	}, true
}

func RedemptionProofPayload(input RedeemInput) string {
	return strings.Join([]string{
		redemptionProofDomain,
		input.ControlPlaneOrigin,
		input.Enrollment,
		input.DevicePublicKey,
		input.Nonce,
	}, "\n")
}

func rotationProofPayload(controlPlaneOrigin string, deviceID, sessionID string, nonce string) string {
	return strings.Join([]string{
		rotationProofDomain,
		controlPlaneOrigin,
		deviceID,
		sessionID,
		nonce,
	}, "\n")
}

func RotationProofPayload(controlPlaneOrigin string, deviceID, sessionID string, nonce string) string {
	return rotationProofPayload(controlPlaneOrigin, deviceID, sessionID, nonce)
}
