package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/olp/internal/secrets"
)

const databasePasswordFile = "postgres_password"

type generatedFile struct {
	name     string
	contents string
	mode     fs.FileMode
}

func initSecrets(args []string, out io.Writer) error {
	f := flag.NewFlagSet("init-secrets", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("dir", "", "directory for OLP secret files")
	databaseDir := f.String("database-dir", "", "directory for the PostgreSQL password file, mounted only by PostgreSQL")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || *dir == "" || *databaseDir == "" {
		return errors.New("usage: olp init-secrets --dir DIR --database-dir DIR")
	}
	databasePassword, err := randomHex()
	if err != nil {
		return err
	}
	if err := writeMissing(*databaseDir, generatedFile{databasePasswordFile, databasePassword, 0444}, out); err != nil {
		return err
	}
	if err := syncSecretDirectory(*databaseDir); err != nil {
		return err
	}
	password, err := os.ReadFile(filepath.Join(*databaseDir, databasePasswordFile))
	if err != nil {
		return errors.New("cannot read the database password file")
	}
	masterKey, err := randomBase64()
	if err != nil {
		return err
	}
	hmacKey, err := randomBase64()
	if err != nil {
		return err
	}
	bootstrapToken, err := randomHex()
	if err != nil {
		return err
	}
	passwordValue := strings.TrimRight(string(password), "\n")
	files := []generatedFile{
		{"olp_master_key", fmt.Sprintf(`{"active_version":1,"keys":[{"version":1,"key":%q}]}`, masterKey), 0400},
		{"olp_auth_hmac_key", hmacKey, 0400},
		{"olp_bootstrap_token", bootstrapToken, 0400},
		{databasePasswordFile, passwordValue, 0400},
		{"pgpass", "*:*:*:*:" + pgpassEscape(passwordValue), 0400},
	}
	for _, file := range files {
		if err := writeMissing(*dir, file, out); err != nil {
			return err
		}
	}
	return syncSecretDirectory(*dir)
}

func setupToken(getenv func(string) string, out io.Writer) error {
	token, ok := secrets.ReadBootstrapToken(getenv("OLP_BOOTSTRAP_TOKEN_FILE"))
	if !ok {
		return errors.New("no bootstrap token is configured: OLP_BOOTSTRAP_TOKEN_FILE must name a readable 32-256 byte secret file")
	}
	_, err := fmt.Fprintf(out, "Open %s/setup and paste this one-time setup token:\n%s\n", strings.TrimSuffix(getenv("OLP_PUBLIC_ORIGIN"), "/"), token)
	return err
}

func writeMissing(dir string, file generatedFile, out io.Writer) error {
	target := filepath.Join(dir, file.name)
	if err := checkExistingSecret(target); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+file.name+"-")
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", target, err)
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.WriteString(file.contents + "\n")
	if err := errors.Join(writeErr, tmp.Chmod(file.mode), tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("cannot write %s: %w", target, err)
	}
	if err := os.Link(tmp.Name(), target); errors.Is(err, fs.ErrExist) {
		return checkExistingSecret(target)
	} else if err != nil {
		return fmt.Errorf("cannot create %s: %w", target, err)
	}
	_, err = fmt.Fprintf(out, "created %s\n", target)
	return err
}

func checkExistingSecret(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cannot keep %s: expected a regular file", path)
	}
	return nil
}

func syncSecretDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open secret directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("cannot sync secret directory: %w", err)
	}
	return nil
}

func randomBytes() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.New("cannot generate random secret material")
	}
	return b, nil
}

func randomHex() (string, error) {
	b, err := randomBytes()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomBase64() (string, error) {
	b, err := randomBytes()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func pgpassEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(value)
}
