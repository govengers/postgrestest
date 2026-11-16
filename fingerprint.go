package postgrestest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
)

// FingerprintFS returns a digest of the names and contents of every file in fsys, for
// Options.Fingerprint: an embed.FS of goose migrations, for example. Any change to a file, a new
// file or a renamed one changes it.
func FingerprintFS(fsys fs.FS) (string, error) {
	digest := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "%d:%s%d:", len(path), path, len(content)) // lengths keep name and content boundaries unambiguous
		digest.Write(content)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint the migrations: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
