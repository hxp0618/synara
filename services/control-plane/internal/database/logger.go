package database

import (
	"log"
	"os"
	"time"

	"gorm.io/gorm/logger"
)

func gormLogger() logger.Interface {
	return logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
		SlowThreshold:             500 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
		Colorful:                  false,
	})
}
