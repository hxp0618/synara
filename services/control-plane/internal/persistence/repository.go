package persistence

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Scope = func(*gorm.DB) *gorm.DB

func TenantScope(tenantID uuid.UUID) Scope {
	return func(db *gorm.DB) *gorm.DB { return db.Where("tenant_id = ?", tenantID) }
}

func ActiveScope(column string) Scope {
	return func(db *gorm.DB) *gorm.DB { return db.Where(column+" = ?", "active") }
}

func NotDeletedScope(column string) Scope {
	return func(db *gorm.DB) *gorm.DB { return db.Where(column + " IS NULL") }
}

type Repository[T any] struct {
	db *gorm.DB
}

func NewRepository[T any](db *gorm.DB) Repository[T] { return Repository[T]{db: db} }

func (r Repository[T]) With(db *gorm.DB) Repository[T] { return Repository[T]{db: db} }

func (r Repository[T]) First(ctx context.Context, scopes ...Scope) (T, error) {
	var item T
	query := r.db.WithContext(ctx).Scopes(scopes...)
	return item, query.First(&item).Error
}

func (r Repository[T]) Find(ctx context.Context, scopes ...Scope) ([]T, error) {
	items := make([]T, 0)
	query := r.db.WithContext(ctx).Scopes(scopes...)
	return items, query.Find(&items).Error
}

func (r Repository[T]) Create(ctx context.Context, item *T) error {
	return r.db.WithContext(ctx).Create(item).Error
}

func InTransaction(ctx context.Context, db *gorm.DB, operation func(*gorm.DB) error) error {
	return db.WithContext(ctx).Transaction(operation)
}
