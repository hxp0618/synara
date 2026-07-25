package leadership

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fencedMutationRecord struct {
	ID    uint `gorm:"primaryKey"`
	Value string
}

func TestWithFenceRejectsStaleLeaderMutationAfterTakeover(t *testing.T) {
	db := openSQLiteTestDB(t)
	if err := db.AutoMigrate(&fencedMutationRecord{}); err != nil {
		t.Fatal(err)
	}
	first := mustNewService(t, db, "fenced-holder-a", time.Second)
	second := mustNewService(t, db, "fenced-holder-b", time.Second)
	leaseName := "fenced-mutation"

	firstLease, held, err := first.Acquire(context.Background(), leaseName)
	if err != nil || !held {
		t.Fatalf("initial acquire held=%v err=%v", held, err)
	}
	firstContext := first.WithFence(context.Background(), firstLease)
	record := fencedMutationRecord{ID: 1, Value: "first-epoch"}
	if err := db.WithContext(firstContext).Create(&record).Error; err != nil {
		t.Fatalf("current leader create: %v", err)
	}

	expireLease(t, db, leaseName, firstLease.AcquiredAt.Add(-time.Second))
	secondLease, held, err := second.Acquire(context.Background(), leaseName)
	if err != nil || !held || secondLease.FencingToken != firstLease.FencingToken+1 {
		t.Fatalf("takeover lease=%#v held=%v err=%v", secondLease, held, err)
	}

	err = db.WithContext(firstContext).Model(&fencedMutationRecord{}).
		Where("id = ?", record.ID).
		Update("value", "stale-write").Error
	if !errors.Is(err, ErrFenceInactive) {
		t.Fatalf("stale leader update error = %v, want %v", err, ErrFenceInactive)
	}
	var stored fencedMutationRecord
	if err := db.First(&stored, record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Value != "first-epoch" {
		t.Fatalf("stale leader changed value to %q", stored.Value)
	}

	secondContext := second.WithFence(context.Background(), secondLease)
	if err := db.WithContext(secondContext).Model(&fencedMutationRecord{}).
		Where("id = ?", record.ID).
		Update("value", "second-epoch").Error; err != nil {
		t.Fatalf("current leader update: %v", err)
	}
	if err := db.First(&stored, record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Value != "second-epoch" {
		t.Fatalf("current leader value = %q", stored.Value)
	}
}
