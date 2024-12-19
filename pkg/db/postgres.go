//go:build postgres

package db

import (
	"gorm.io/driver/postgres"
)

func init() {
	Factory["postgres"] = postgres.Open
}
