package database

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type Options struct {
	MaxOpenConnections    int
	MaxIdleConnections    int
	ConnectionMaxLifetime time.Duration
	ConnectionMaxIdleTime time.Duration
	MigrationLockTimeout  time.Duration
}

func DefaultOptions() Options {
	return Options{
		MaxOpenConnections:    20,
		MaxIdleConnections:    5,
		ConnectionMaxLifetime: time.Hour,
		ConnectionMaxIdleTime: 15 * time.Minute,
		MigrationLockTimeout:  30 * time.Second,
	}
}

func resolveOptions(values []Options) Options {
	if len(values) == 0 {
		return DefaultOptions()
	}
	return values[0]
}

func Open(ctx context.Context, databaseURL string, values ...Options) (*gorm.DB, error) {
	options := resolveOptions(values)
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		TranslateError:         true,
		SkipDefaultTransaction: true,
		Logger:                 gormLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open database through GORM: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("resolve database pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(options.MaxOpenConnections)
	sqlDB.SetMaxIdleConns(options.MaxIdleConnections)
	sqlDB.SetConnMaxLifetime(options.ConnectionMaxLifetime)
	sqlDB.SetConnMaxIdleTime(options.ConnectionMaxIdleTime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}
