package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/kmsworker"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	command := "serve"
	if len(arguments) > 0 {
		command = strings.TrimSpace(arguments[0])
		arguments = arguments[1:]
	}
	if len(arguments) != 0 {
		return errors.New("kms-worker accepts only one optional command: serve or reseal")
	}
	config, err := kmsworker.LoadRuntimeConfig()
	if err != nil {
		return fmt.Errorf("load KMS worker configuration: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	db, err := kmsworker.OpenStore(ctx, config.Store)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if err := kmsworker.MigrateStore(ctx, db); err != nil {
		return err
	}
	seals, err := kmsworker.NewSealKeyring(config.SealKey, config.FallbackSealKeys...)
	zero(config.SealKey)
	for index := range config.FallbackSealKeys {
		zero(config.FallbackSealKeys[index])
	}
	if err != nil {
		return fmt.Errorf("configure KMS sealing keyring: %w", err)
	}
	service, err := kmsworker.NewService(db, seals, kmsworker.ServiceOptions{
		MinimumDeleteDelay: config.MinimumDeleteDelay, InventoryMaximumAge: config.InventoryMaximumAge,
	})
	if err != nil {
		return err
	}
	switch command {
	case "reseal":
		operatorReference := strings.TrimSpace(os.Getenv("SYNARA_KMS_RESEAL_OPERATOR_REFERENCE"))
		actorIdentity := strings.TrimSpace(os.Getenv("SYNARA_KMS_RESEAL_ACTOR_IDENTITY"))
		if actorIdentity == "" {
			return errors.New("SYNARA_KMS_RESEAL_ACTOR_IDENTITY is required for reseal")
		}
		receipt, err := service.ResealAll(ctx, kmsworker.Actor{Identity: actorIdentity, Role: kmsworker.RoleKeyManager}, operatorReference)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	case "serve":
		return serve(ctx, config, service)
	default:
		return fmt.Errorf("unsupported kms-worker command %q", command)
	}
}

func serve(ctx context.Context, config kmsworker.RuntimeConfig, service *kmsworker.Service) error {
	authorizer, err := kmsworker.NewAuthorizer(config.IdentityRoles)
	if err != nil {
		return err
	}
	handler, err := kmsworker.NewHTTPServer(service, authorizer)
	if err != nil {
		return err
	}
	tlsConfig, err := config.ServerTLSConfig()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for KMS requests: %w", err)
	}
	tlsListener := tls.NewListener(listener, tlsConfig)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         tlsConfig,
	}
	serveErrors := make(chan error, 1)
	go func() {
		slog.Info("KMS worker listening", "address", config.ListenAddress)
		serveErrors <- server.Serve(tlsListener)
	}()
	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down KMS worker: %w", err)
		}
		return nil
	}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
