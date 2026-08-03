package database

import (
	"crypto/sha256"
	"database/sql/driver"
	"fmt"

	sqlite3 "github.com/glebarez/go-sqlite"
)

func init() {
	sqlite3.MustRegisterDeterministicScalarFunction(
		"synara_sha256",
		1,
		func(_ *sqlite3.FunctionContext, arguments []driver.Value) (driver.Value, error) {
			if len(arguments) != 1 {
				return nil, fmt.Errorf("synara_sha256 expects one argument")
			}
			var payload []byte
			switch value := arguments[0].(type) {
			case []byte:
				payload = value
			case string:
				payload = []byte(value)
			default:
				return nil, fmt.Errorf("synara_sha256 expects blob or text, got %T", value)
			}
			digest := sha256.Sum256(payload)
			return digest[:], nil
		},
	)
}
