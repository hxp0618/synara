package containmentattestation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const (
	SchemaVersionV1 = 1
	signatureDomain = "synara.process-containment-attestation.v1\n"
)

// Envelope is carried by the Worker registration Manifest. The signature is
// verified against a server-authoritative Execution Target public key; the
// private key must remain available only to the protected supervisor.
type Envelope struct {
	SchemaVersion int    `json:"schemaVersion"`
	KeyID         string `json:"keyId"`
	Signature     string `json:"signature"`
}

// Statement binds process-containment evidence to one physical Worker
// identity and one immutable Worker image. All fields are covered by the
// Ed25519 signature; none are accepted from the Envelope itself.
type Statement struct {
	ExecutionTargetID  string `json:"executionTargetId"`
	TargetKind         string `json:"targetKind"`
	InstanceUID        string `json:"instanceUid"`
	ClusterID          string `json:"clusterId"`
	Namespace          string `json:"namespace"`
	PodName            string `json:"podName"`
	WorkerBuildVersion string `json:"workerBuildVersion"`
	WorkerBuildGitSHA  string `json:"workerBuildGitSha"`
	ImageDigest        string `json:"imageDigest"`
	OperatingSystem    string `json:"operatingSystem"`
	Architecture       string `json:"architecture"`
	Mode               string `json:"mode"`
	SupervisorVersion  string `json:"supervisorVersion"`
	ProbeVersion       int    `json:"probeVersion"`
	ProbeSHA256        string `json:"probeSha256"`
	SupervisorIdentity string `json:"supervisorIdentity"`
	ProviderIdentity   string `json:"providerIdentity"`
}

type signedPayload struct {
	SchemaVersion int       `json:"schemaVersion"`
	KeyID         string    `json:"keyId"`
	Statement     Statement `json:"statement"`
}

func Sign(privateKey ed25519.PrivateKey, keyID string, statement Statement) (Envelope, error) {
	keyID = strings.TrimSpace(keyID)
	if len(privateKey) != ed25519.PrivateKeySize || keyID == "" {
		return Envelope{}, errors.New("invalid process-containment signing key")
	}
	payload, err := encodePayload(keyID, statement)
	if err != nil {
		return Envelope{}, err
	}
	signature := ed25519.Sign(privateKey, payload)
	return Envelope{
		SchemaVersion: SchemaVersionV1,
		KeyID:         keyID,
		Signature:     base64.StdEncoding.EncodeToString(signature),
	}, nil
}

func Verify(publicKey ed25519.PublicKey, envelope Envelope, statement Statement) error {
	if len(publicKey) != ed25519.PublicKeySize || envelope.SchemaVersion != SchemaVersionV1 ||
		envelope.KeyID == "" || envelope.KeyID != strings.TrimSpace(envelope.KeyID) ||
		envelope.Signature == "" || envelope.Signature != strings.TrimSpace(envelope.Signature) {
		return errors.New("invalid process-containment attestation envelope")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		base64.StdEncoding.EncodeToString(signature) != envelope.Signature {
		return errors.New("invalid process-containment attestation signature encoding")
	}
	payload, err := encodePayload(envelope.KeyID, statement)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return errors.New("process-containment attestation signature verification failed")
	}
	return nil
}

func PublicKeySHA256(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", errors.New("invalid process-containment public key")
	}
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:]), nil
}

func encodePayload(keyID string, statement Statement) ([]byte, error) {
	encoded, err := json.Marshal(signedPayload{
		SchemaVersion: SchemaVersionV1,
		KeyID:         keyID,
		Statement:     statement,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(signatureDomain), encoded...), nil
}
