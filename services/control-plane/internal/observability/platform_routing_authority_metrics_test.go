package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPlatformRoutingPublicationMetricsUseImmutableReceiptCount(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:platform-routing-metrics?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.ExecutionTarget{}, &persistence.PlatformRoutingPublication{}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 13, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "platform-routing-metrics", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := db.Create(&persistence.PlatformRoutingPublication{
			PublisherIdentity: "metrics-publisher", Nonce: uuid.New(), KeyID: "key-v1",
			PublicKeySHA256: strings.Repeat("a", 64), ExecutionTargetID: targetID,
			RequestSHA256: strings.Repeat("b", 64), ObservedAt: now.Add(time.Duration(index) * time.Second),
			Response: map[string]any{"replayed": false}, ResponseSHA256: strings.Repeat("c", 64),
			ReceivedAt: now.Add(time.Duration(index) * time.Second),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := New(db).writePlatformRoutingPublicationMetrics(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"# TYPE synara_platform_routing_publications_total counter",
		"synara_platform_routing_publications_total 2",
		"# TYPE synara_platform_routing_publication_latest_timestamp_seconds gauge",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, output.String())
		}
	}
}
