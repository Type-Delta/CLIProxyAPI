package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func BackupID(manifest BackupManifest) (string, error) {
	digest, err := hex.DecodeString(manifest.DatabaseSHA256)
	if err != nil || len(digest) != sha256.Size {
		return "", fmt.Errorf("backup manifest database digest is invalid")
	}
	return "backup-" + manifest.DatabaseSHA256, nil
}
