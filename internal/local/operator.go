package local

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type OperatorAccess struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Login          string `json:"login"`
	Password       string `json:"password"`
}

func ProvisionOperator(root string) error {
	path := filepath.Join(root, "operator-access.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var seed Seed
	if err := Read(filepath.Join(root, "seed.json"), &seed); err != nil {
		return err
	}
	return writeJSON(
		path,
		OperatorAccess{
			OrganizationID: seed.OrganizationID,
			UserID:         uuid.NewString(),
			Login:          "operator@thermora.local",
			Password:       Secret(),
		},
	)
}
