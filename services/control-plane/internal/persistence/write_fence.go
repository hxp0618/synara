package persistence

import (
	"context"
	"errors"
	"sync"

	"gorm.io/gorm"
)

const writeFencePluginName = "synara:transaction-write-fence"

const writeFenceStartedTransactionKey = "synara:transaction-write-fence:started-transaction"

// TransactionWriteFence verifies an external authority using the same
// database transaction that is about to perform an authoritative mutation.
// The callback must lock any epoch row that prevents concurrent takeover.
type TransactionWriteFence func(context.Context, *gorm.DB) error

type transactionWriteFenceContextKey struct{}

func WithTransactionWriteFence(ctx context.Context, fence TransactionWriteFence) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fence == nil {
		return ctx
	}
	return context.WithValue(ctx, transactionWriteFenceContextKey{}, fence)
}

type writeFencePlugin struct{}

func (writeFencePlugin) Name() string { return writeFencePluginName }

func (writeFencePlugin) Initialize(db *gorm.DB) error {
	if db == nil {
		return errors.New("write fence database is required")
	}
	create := db.Callback().Create()
	if err := create.Before("gorm:begin_transaction").Register(writeFencePluginName+":begin-create", beginTransactionWriteFence); err != nil {
		return err
	}
	if err := create.Before("gorm:create").Register(writeFencePluginName+":create", enforceTransactionWriteFence); err != nil {
		return err
	}
	if err := create.After("gorm:commit_or_rollback_transaction").Register(writeFencePluginName+":commit-create", commitOrRollbackTransactionWriteFence); err != nil {
		return err
	}

	update := db.Callback().Update()
	if err := update.Before("gorm:begin_transaction").Register(writeFencePluginName+":begin-update", beginTransactionWriteFence); err != nil {
		return err
	}
	if err := update.Before("gorm:update").Register(writeFencePluginName+":update", enforceTransactionWriteFence); err != nil {
		return err
	}
	if err := update.After("gorm:commit_or_rollback_transaction").Register(writeFencePluginName+":commit-update", commitOrRollbackTransactionWriteFence); err != nil {
		return err
	}

	remove := db.Callback().Delete()
	if err := remove.Before("gorm:begin_transaction").Register(writeFencePluginName+":begin-delete", beginTransactionWriteFence); err != nil {
		return err
	}
	if err := remove.Before("gorm:delete").Register(writeFencePluginName+":delete", enforceTransactionWriteFence); err != nil {
		return err
	}
	if err := remove.After("gorm:commit_or_rollback_transaction").Register(writeFencePluginName+":commit-delete", commitOrRollbackTransactionWriteFence); err != nil {
		return err
	}
	return nil
}

func transactionWriteFence(db *gorm.DB) (TransactionWriteFence, bool) {
	if db == nil || db.Statement == nil || db.Statement.Context == nil {
		return nil, false
	}
	fence, ok := db.Statement.Context.Value(transactionWriteFenceContextKey{}).(TransactionWriteFence)
	return fence, ok && fence != nil
}

func beginTransactionWriteFence(db *gorm.DB) {
	if db == nil || db.Error != nil {
		return
	}
	if _, fenced := transactionWriteFence(db); !fenced {
		return
	}
	if !db.Config.SkipDefaultTransaction {
		// GORM's built-in transaction callbacks already bracket the fence and
		// mutation when default transactions are enabled.
		return
	}
	if _, alreadyTransactional := db.Statement.ConnPool.(gorm.TxCommitter); alreadyTransactional {
		return
	}
	tx := db.Begin()
	if tx.Error != nil {
		if !errors.Is(tx.Error, gorm.ErrInvalidTransaction) {
			db.AddError(tx.Error)
		}
		return
	}
	db.Statement.ConnPool = tx.Statement.ConnPool
	db.InstanceSet(writeFenceStartedTransactionKey, true)
}

func commitOrRollbackTransactionWriteFence(db *gorm.DB) {
	if db == nil || db.Statement == nil {
		return
	}
	if _, started := db.InstanceGet(writeFenceStartedTransactionKey); !started {
		return
	}
	if db.Error != nil {
		db.Rollback()
	} else {
		db.Commit()
	}
	db.Statement.ConnPool = db.ConnPool
}

func enforceTransactionWriteFence(db *gorm.DB) {
	if db == nil || db.Statement == nil || db.Statement.Context == nil || db.Error != nil {
		return
	}
	fence, ok := transactionWriteFence(db)
	if !ok {
		return
	}
	// NewDB clears the caller's Model, clauses, and destination while retaining
	// the current transaction's ConnPool. The fence row lock is therefore held
	// until the exact mutation transaction commits or rolls back.
	// The no-op predicate forces GORM to materialize the NewDB clone before a
	// later WithContext call; otherwise it can clone the caller's current table
	// back into the fence query while a callback is executing.
	clean := db.Session(&gorm.Session{NewDB: true}).Where("1 = 1").WithContext(db.Statement.Context)
	if err := fence(db.Statement.Context, clean); err != nil {
		db.AddError(err)
	}
}

var installWriteFenceMu sync.Mutex

func InstallTransactionWriteFence(db *gorm.DB) error {
	if db == nil {
		return errors.New("write fence database is required")
	}
	installWriteFenceMu.Lock()
	defer installWriteFenceMu.Unlock()
	if _, installed := db.Config.Plugins[writeFencePluginName]; installed {
		return nil
	}
	return db.Use(writeFencePlugin{})
}
