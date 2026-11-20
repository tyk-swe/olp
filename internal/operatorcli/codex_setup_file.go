package operatorcli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type codexFile struct {
	path string
	data []byte
	info os.FileInfo
}

func readCodexFile(path string) (codexFile, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return codexFile{path: path}, nil
	}
	if err != nil {
		return codexFile{}, err
	}
	if !info.Mode().IsRegular() {
		return codexFile{}, errors.New("Codex config must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return codexFile{}, err
	}
	defer file.Close()

	current, err := file.Stat()
	if err != nil {
		return codexFile{}, err
	}
	if !os.SameFile(info, current) {
		return codexFile{}, errors.New("Codex config changed while reading it; retry the command")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCodexConfig+1))
	if err != nil {
		return codexFile{}, err
	}
	if len(data) > maxCodexConfig {
		return codexFile{}, errors.New("Codex config exceeds 4 MiB")
	}
	return codexFile{path: path, data: data, info: current}, nil
}

func (file codexFile) unchanged() error {
	current, err := readCodexFile(file.path)
	if err != nil {
		return err
	}
	sameFile := file.info == nil && current.info == nil ||
		file.info != nil && current.info != nil && os.SameFile(file.info, current.info) && file.info.Mode() == current.info.Mode()
	if !sameFile || !bytes.Equal(file.data, current.data) {
		return errors.New("Codex config changed while preparing the write; retry the command")
	}
	return nil
}

func (file codexFile) replace(data []byte) error {
	if bytes.Equal(file.data, data) {
		return nil
	}
	dir := filepath.Dir(file.path)
	if file.info == nil {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".olp-code-setup-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if file.info != nil {
		if err := tmp.Chmod(file.info.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := file.unchanged(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file.path)
}

func (file codexFile) detach(doc codexDocument, state codexSetupState) error {
	if err := state.checkDrift(doc); err != nil {
		return err
	}
	data := doc.edit(state.Previous, "", state.Joined)
	if _, err := parseCodexTOML(data); err != nil {
		return errors.New("restoring the old settings would conflict with the current Codex config; no settings were changed")
	}
	if state.Created && len(data) == 0 {
		if err := file.unchanged(); err != nil {
			return err
		}
		return os.Remove(file.path)
	}
	return file.replace(data)
}
