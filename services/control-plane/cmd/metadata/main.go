package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/kmsrotation"
	"github.com/synara-ai/synara/services/control-plane/internal/metadatamigration"
	"github.com/synara-ai/synara/services/control-plane/internal/runtimekeys"
	"github.com/synara-ai/synara/services/control-plane/internal/runtimesecretrotation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: control-plane-metadata export --output manifest.json | import --input manifest.json | rewrap-kms [--execute --operator REF] | rekey-runtime-secrets [--execute --operator REF]")
	}
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		fatal(err.Error())
	}
	store, err := database.OpenMetadataStore(ctx, cfg.Platform, cfg.DatabaseURL, cfg.SQLitePath, database.Options{
		MaxOpenConnections: cfg.DatabaseMaxOpenConnections, MaxIdleConnections: cfg.DatabaseMaxIdleConnections,
		ConnectionMaxLifetime: cfg.DatabaseConnectionMaxLifetime, ConnectionMaxIdleTime: cfg.DatabaseConnectionMaxIdleTime,
		MigrationLockTimeout: cfg.DatabaseMigrationLockTimeout,
	})
	if err != nil {
		fatal(err.Error())
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		fatal(err.Error())
	}

	switch os.Args[1] {
	case "export":
		flags := flag.NewFlagSet("export", flag.ExitOnError)
		output := flags.String("output", "", "metadata manifest output path")
		_ = flags.Parse(os.Args[2:])
		if *output == "" {
			fatal("--output is required")
		}
		manifest, err := metadatamigration.Export(ctx, store.DB(), cfg.Platform.Profile)
		if err != nil {
			fatal(err.Error())
		}
		artifactStore, err := artifacts.NewStore(ctx, cfg)
		if err != nil {
			fatal(err.Error())
		}
		if err := metadatamigration.ValidateArtifactPayloads(ctx, manifest, artifactStore); err != nil {
			fatal(err.Error())
		}
		encoded, err := metadatamigration.Encode(manifest)
		if err != nil {
			fatal(err.Error())
		}
		if err := os.WriteFile(*output, encoded, 0o600); err != nil {
			fatal(err.Error())
		}
		printJSON(map[string]any{
			"manifestId": manifest.ManifestID, "output": *output,
			"artifactPayloadMigration": "references_validated",
			"artifactPayloadCount":     len(manifest.Artifacts.Entries),
		})
	case "import":
		flags := flag.NewFlagSet("import", flag.ExitOnError)
		input := flags.String("input", "", "metadata manifest input path")
		sourceArtifactDir := flags.String("source-artifact-dir", "", "personal Local Artifact root for payload migration")
		_ = flags.Parse(os.Args[2:])
		if *input == "" {
			fatal("--input is required")
		}
		encoded, err := os.ReadFile(*input)
		if err != nil {
			fatal(err.Error())
		}
		manifest, err := metadatamigration.Decode(encoded)
		if err != nil {
			fatal(err.Error())
		}
		report, err := metadatamigration.Import(ctx, store.DB(), cfg.Platform, manifest, encoded)
		if err != nil {
			fatal(err.Error())
		}
		if len(manifest.Artifacts.Entries) > 0 && *sourceArtifactDir != "" {
			source, err := artifacts.NewLocalStore(*sourceArtifactDir)
			if err != nil {
				fatal(err.Error())
			}
			destination, err := artifacts.NewStore(ctx, cfg)
			if err != nil {
				fatal(err.Error())
			}
			payloadReport, err := metadatamigration.MigrateArtifactPayloads(ctx, store.DB(), manifest, source, destination)
			if err != nil {
				fatal(err.Error())
			}
			report.ArtifactPayloadMigration = "completed"
			printJSON(map[string]any{"metadata": report, "artifacts": payloadReport})
			return
		}
		printJSON(report)
	case "rewrap-kms":
		flags := flag.NewFlagSet("rewrap-kms", flag.ExitOnError)
		execute := flags.Bool("execute", false, "execute the rewrap; omission performs a read-only dry-run")
		operator := flags.String("operator", "", "non-secret change or incident reference (required with --execute)")
		batchSize := flags.Int("batch-size", 200, "rows per deterministic batch (1-1000)")
		resumeRunID := flags.String("resume-run-id", "", "resume an incomplete immutable run")
		_ = flags.Parse(os.Args[2:])
		cipher, err := credentialkms.New(ctx, credentialkms.Config{
			Provider: cfg.CredentialKMSProvider, KeyID: cfg.CredentialKMSKeyID,
			LocalKey: cfg.CredentialKMSLocalKey, Region: cfg.CredentialKMSAWSRegion,
			DecryptKeys: metadataCredentialKMSDecryptKeys(cfg.CredentialKMSDecryptKeys),
		})
		if err != nil {
			fatal(err.Error())
		}
		service, err := kmsrotation.New(store.DB(), cipher)
		if err != nil {
			fatal(err.Error())
		}
		if !*execute {
			if *resumeRunID != "" {
				fatal("--resume-run-id requires --execute")
			}
			plan, err := service.Plan(ctx)
			if err != nil {
				fatal(err.Error())
			}
			printJSON(map[string]any{"mode": "dry-run", "plan": plan})
			return
		}
		var parsedResumeRunID *uuid.UUID
		if *resumeRunID != "" {
			value, err := uuid.Parse(*resumeRunID)
			if err != nil {
				fatal("--resume-run-id must be a UUID")
			}
			parsedResumeRunID = &value
		}
		report, err := service.Execute(ctx, kmsrotation.ExecuteOptions{
			OperatorReference: *operator, BatchSize: *batchSize, ResumeRunID: parsedResumeRunID,
		})
		if err != nil {
			fatal(err.Error())
		}
		printJSON(map[string]any{"mode": "execute", "receipt": report})
	case "rekey-runtime-secrets":
		flags := flag.NewFlagSet("rekey-runtime-secrets", flag.ExitOnError)
		execute := flags.Bool("execute", false, "execute re-encryption; omission performs a read-only dry-run")
		operator := flags.String("operator", "", "non-secret change or incident reference (required with --execute)")
		batchSize := flags.Int("batch-size", 200, "rows per deterministic batch (1-1000)")
		resumeRunID := flags.String("resume-run-id", "", "resume an incomplete immutable run")
		_ = flags.Parse(os.Args[2:])
		cipher, err := runtimekeys.NewProviderCursorCipher(cfg)
		if err != nil {
			fatal(err.Error())
		}
		service, err := runtimesecretrotation.New(store.DB(), cipher)
		if err != nil {
			fatal(err.Error())
		}
		if !*execute {
			if *resumeRunID != "" {
				fatal("--resume-run-id requires --execute")
			}
			plan, err := service.Plan(ctx, *batchSize)
			if err != nil {
				fatal(err.Error())
			}
			printJSON(map[string]any{"mode": "dry-run", "plan": plan})
			return
		}
		var parsedResumeRunID *uuid.UUID
		if *resumeRunID != "" {
			value, err := uuid.Parse(*resumeRunID)
			if err != nil {
				fatal("--resume-run-id must be a UUID")
			}
			parsedResumeRunID = &value
		}
		report, err := service.Execute(ctx, runtimesecretrotation.ExecuteOptions{
			OperatorReference: *operator, BatchSize: *batchSize, ResumeRunID: parsedResumeRunID,
		})
		if err != nil {
			fatal(err.Error())
		}
		printJSON(map[string]any{"mode": "execute", "receipt": report})
	default:
		fatal(fmt.Sprintf("unknown metadata command %q", os.Args[1]))
	}
}

func metadataCredentialKMSDecryptKeys(values []config.CredentialKMSDecryptKeyConfig) []credentialkms.DecryptKeyConfig {
	result := make([]credentialkms.DecryptKeyConfig, 0, len(values))
	for _, value := range values {
		result = append(result, credentialkms.DecryptKeyConfig{
			Provider: value.Provider, KeyID: value.KeyID, LocalKey: value.LocalKey, Region: value.Region,
		})
	}
	return result
}

func printJSON(value any) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func fatal(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
