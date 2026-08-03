package kms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/kmsworker"
)

func TestSynaraKMSWrapperMTLSAndMixedProviderRewrap(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open("file:synara-kms-adapter?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := kmsworker.MigrateStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	seals, _ := kmsworker.NewSealKeyring(bytes.Repeat([]byte{0x51}, 32))
	service, err := kmsworker.NewService(db, seals, kmsworker.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateKey(ctx, kmsworker.Actor{Identity: "manager", Role: kmsworker.RoleKeyManager}, kmsworker.CreateKeyInput{
		Name: "adapter-test", IdempotencyKey: "create-adapter-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	authorizer, _ := kmsworker.NewAuthorizer(map[string][]kmsworker.Role{
		"spiffe://synara.test/control-plane": {kmsworker.RoleControlPlaneCryptor},
	})
	handler, _ := kmsworker.NewHTTPServer(service, authorizer)
	serverCertificate, clientCertificate, rootPool := testCertificateChain(t)
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(rootPool.certificate)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: serverRoots,
	}
	server.StartTLS()
	defer server.Close()
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: rootPool.pool, Certificates: []tls.Certificate{clientCertificate},
		}},
		Timeout: 5 * time.Second,
	}
	endpoint, _ := url.Parse(server.URL)
	versionOneID := kmsworker.VersionedKeyID(created.KeyID, 1)
	synaraV1, err := newSynaraKeyWrapperWithClient(ctx, versionOneID, endpoint, client, true)
	if err != nil {
		t.Fatal(err)
	}

	dataKey := bytes.Repeat([]byte{0x62}, 32)
	aad := []byte("adapter-aad")
	wrapped, err := synaraV1.WrapKey(ctx, dataKey, aad)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := synaraV1.UnwrapKey(ctx, wrapped, aad)
	if err != nil || !bytes.Equal(plaintext, dataKey) {
		t.Fatalf("wrapper round trip = %x, %v", plaintext, err)
	}

	localOld, _ := NewLocalKeyWrapper("local-old", bytes.Repeat([]byte{0x31}, 32))
	oldLocalCipher := NewEnvelopeCipher(localOld)
	oldLocalEnvelope, err := oldLocalCipher.Encrypt(ctx, []byte("credential-payload"), aad)
	if err != nil {
		t.Fatal(err)
	}
	toSynara, err := NewEnvelopeCipherWithDecryptors(synaraV1, localOld)
	if err != nil {
		t.Fatal(err)
	}
	synaraEnvelope, changed, err := toSynara.RewrapToPrimary(ctx, oldLocalEnvelope, aad)
	if err != nil || !changed || synaraEnvelope.KMSProvider != "synara-kms" || synaraEnvelope.KMSKeyID != versionOneID {
		t.Fatalf("local -> synara rewrap = %#v, %v, %v", synaraEnvelope, changed, err)
	}
	if !bytes.Equal(synaraEnvelope.EncryptedPayload, oldLocalEnvelope.EncryptedPayload) {
		t.Fatal("local -> synara rewrap changed business ciphertext")
	}

	localNew, _ := NewLocalKeyWrapper("local-new", bytes.Repeat([]byte{0x32}, 32))
	backToLocal, err := NewEnvelopeCipherWithDecryptors(localNew, synaraV1)
	if err != nil {
		t.Fatal(err)
	}
	localEnvelope, changed, err := backToLocal.RewrapToPrimary(ctx, synaraEnvelope, aad)
	if err != nil || !changed || localEnvelope.KMSProvider != "local" {
		t.Fatalf("synara -> local rewrap = %#v, %v, %v", localEnvelope, changed, err)
	}

	awsOld := providerOverrideWrapper{KeyWrapper: localOld, provider: "aws-kms", keyID: "arn:aws:kms:test:old"}
	awsCipher := NewEnvelopeCipher(awsOld)
	awsEnvelope, err := awsCipher.Encrypt(ctx, []byte("aws-payload"), aad)
	if err != nil {
		t.Fatal(err)
	}
	fromAWS, err := NewEnvelopeCipherWithDecryptors(synaraV1, awsOld)
	if err != nil {
		t.Fatal(err)
	}
	fromAWSEnvelope, changed, err := fromAWS.RewrapToPrimary(ctx, awsEnvelope, aad)
	if err != nil || !changed || fromAWSEnvelope.KMSProvider != "synara-kms" {
		t.Fatalf("aws -> synara rewrap = %#v, %v, %v", fromAWSEnvelope, changed, err)
	}
	awsNew := providerOverrideWrapper{KeyWrapper: localNew, provider: "aws-kms", keyID: "arn:aws:kms:test:new"}
	toAWS, err := NewEnvelopeCipherWithDecryptors(awsNew, synaraV1)
	if err != nil {
		t.Fatal(err)
	}
	finalEnvelope, changed, err := toAWS.RewrapToPrimary(ctx, fromAWSEnvelope, aad)
	if err != nil || !changed || finalEnvelope.KMSProvider != "aws-kms" {
		t.Fatalf("synara -> aws rewrap = %#v, %v, %v", finalEnvelope, changed, err)
	}

	rotated, err := service.RotateKey(ctx, kmsworker.Actor{Identity: "manager", Role: kmsworker.RoleKeyManager}, created.KeyID, kmsworker.RotateKeyInput{
		OperatorReference: "rotation-test", IdempotencyKey: "rotate-adapter-test",
	})
	if err != nil || rotated.ActiveVersion != 2 {
		t.Fatalf("rotate = %#v, %v", rotated, err)
	}
	if _, err := newSynaraKeyWrapperWithClient(ctx, versionOneID, endpoint, client, true); err == nil {
		t.Fatal("decrypt-only version was accepted as primary")
	}
	if _, err := newSynaraKeyWrapperWithClient(ctx, versionOneID, endpoint, client, false); err != nil {
		t.Fatalf("decrypt-only version was rejected as fallback: %v", err)
	}
}

type providerOverrideWrapper struct {
	KeyWrapper
	provider string
	keyID    string
}

func (w providerOverrideWrapper) Provider() string { return w.provider }
func (w providerOverrideWrapper) KeyID() string    { return w.keyID }

type testRoot struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	pool        *x509.CertPool
}

func testCertificateChain(t *testing.T) (tls.Certificate, tls.Certificate, testRoot) {
	t.Helper()
	now := time.Now().Add(-time.Hour)
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-root"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCertificate, _ := x509.ParseCertificate(rootDER)
	pool := x509.NewCertPool()
	pool.AddCert(rootCertificate)
	root := testRoot{certificate: rootCertificate, privateKey: rootKey, pool: pool}
	server := issueCertificate(t, root, "server", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"127.0.0.1", "localhost"})
	identity, _ := url.Parse("spiffe://synara.test/control-plane")
	client := issueCertificate(t, root, "client", []*url.URL{identity}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	return server, client, root
}

func issueCertificate(t *testing.T, root testRoot, name string, uris []*url.URL, usages []x509.ExtKeyUsage, hosts []string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages, URIs: uris,
	}
	for _, host := range hosts {
		if host == "127.0.0.1" {
			template.IPAddresses = append(template.IPAddresses, []byte{127, 0, 0, 1})
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root.certificate, &key.PublicKey, root.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
