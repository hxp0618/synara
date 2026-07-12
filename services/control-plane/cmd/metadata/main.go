package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/metadatamigration"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: control-plane-metadata export --output manifest.json | import --input manifest.json")
	}
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		fatal(err.Error())
	}
	store, err := database.OpenMetadataStore(ctx, cfg.Platform, cfg.DatabaseURL, cfg.SQLitePath)
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
	default:
		fatal(fmt.Sprintf("unknown metadata command %q", os.Args[1]))
	}
}

func printJSON(value any) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func fatal(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
