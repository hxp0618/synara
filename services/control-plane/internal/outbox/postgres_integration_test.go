package outbox

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentClaimAndExpiredRecovery(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	message := persistence.OutboxMessage{
		ID: uuid.New(), Topic: "outbox.integration." + uuid.NewString(), MessageKey: uuid.NewString(),
		Payload: map[string]any{"test": true}, Headers: map[string]any{"eventVersion": 1},
		AvailableAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), CreatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&persistence.OutboxMessage{}, "id = ?", message.ID).Error })

	first := integrationService(t, db, "first", now)
	second := integrationService(t, db, "second", now)
	start := make(chan struct{})
	results := make(chan []Message, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, service := range []*Service{first, second} {
		wait.Add(1)
		go func(service *Service) {
			defer wait.Done()
			<-start
			messages, err := service.Claim(context.Background())
			results <- messages
			errors <- err
		}(service)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	claimed := 0
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for messages := range results {
		claimed += len(messages)
	}
	if claimed != 1 {
		t.Fatalf("concurrent dispatchers claimed the message %d times", claimed)
	}

	recovery := integrationService(t, db, "recovery", now.Add(31*time.Second))
	messages, err := recovery.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != message.ID {
		t.Fatalf("expired PostgreSQL claim was not recovered: %#v", messages)
	}
}

func integrationService(t *testing.T, db *gorm.DB, instance string, now time.Time) *Service {
	t.Helper()
	service, err := NewService(db, Config{
		InstanceID: instance, BatchSize: 1, ClaimTTL: 30 * time.Second,
		MaxAttempts: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
