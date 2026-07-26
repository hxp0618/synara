package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

const maximumPublicationBytes = 1 << 20

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("routing-authority-sign", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	privateKeyPath := flags.String("private-key-file", "", "PKCS#8 PEM Ed25519 private key file")
	publicKeyOnly := flags.Bool("public-key-only", false, "print the padded-base64 public key and exit")
	allowKeySymlink := flags.Bool("allow-key-symlink", false, "allow a symlinked key from a read-only projected secret volume")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() != 0 || strings.TrimSpace(*privateKeyPath) == "" {
		return errors.New("--private-key-file is required and positional arguments are not supported")
	}
	privateKey, err := loadPrivateKey(strings.TrimSpace(*privateKeyPath), *allowKeySymlink)
	if err != nil {
		return err
	}
	if *publicKeyOnly {
		publicKey, ok := privateKey.Public().(ed25519.PublicKey)
		if !ok || len(publicKey) != ed25519.PublicKeySize {
			return errors.New("derive Ed25519 public key")
		}
		_, err := fmt.Fprintln(output, base64.StdEncoding.EncodeToString(publicKey))
		return err
	}

	encodedInput, err := io.ReadAll(io.LimitReader(input, maximumPublicationBytes+1))
	if err != nil {
		return fmt.Errorf("read unsigned publication: %w", err)
	}
	if len(encodedInput) > maximumPublicationBytes {
		return errors.New("unsigned publication exceeds the 1 MiB limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(encodedInput)))
	decoder.DisallowUnknownFields()
	var publication routing.PlatformAuthorityPublication
	if err := decoder.Decode(&publication); err != nil {
		return fmt.Errorf("decode unsigned publication: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unsigned publication must contain exactly one JSON value")
	}
	if strings.TrimSpace(publication.Signature) != "" {
		return errors.New("unsigned publication must omit signature or set it to an empty string")
	}
	signed, err := routing.SignPlatformAuthorityPublication(privateKey, publication)
	if err != nil {
		return fmt.Errorf("sign Platform routing publication: %w", err)
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(signed); err != nil {
		return fmt.Errorf("encode signed publication: %w", err)
	}
	return nil
}

func loadPrivateKey(path string, allowSymlink bool) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !allowSymlink {
			return nil, errors.New("private key path is a symbolic link; use --allow-key-symlink only for a read-only projected secret volume")
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve private key symlink: %w", resolveErr)
		}
		path = resolved
		info, err = os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect resolved private key file: %w", err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("private key path must resolve to a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("private key file must not be writable by group or other users")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	block, trailing := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(trailing))) != 0 {
		return nil, errors.New("private key file must contain exactly one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key file does not contain a valid PKCS#8 key")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("private key must be Ed25519")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), nil
}
