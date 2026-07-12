package persistence

import (
	"errors"

	"gorm.io/gorm"
)

func IsConstraintViolation(err error) bool {
	return errors.Is(err, gorm.ErrCheckConstraintViolated) ||
		errors.Is(err, gorm.ErrForeignKeyViolated) ||
		errors.Is(err, gorm.ErrDuplicatedKey)
}
