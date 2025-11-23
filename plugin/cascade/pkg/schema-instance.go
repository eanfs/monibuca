package cascade

import (
	"database/sql"

	"github.com/quic-go/quic-go"
	"gorm.io/gorm"
)

type Instance struct {
	gorm.Model
	Name       string
	Secret     sql.NullString `gorm:"unique;index:idx_secret"`
	IP         string
	Online     bool
	*quic.Conn `gorm:"-"`
}

func (i *Instance) GetKey() uint {
	return i.ID
}
