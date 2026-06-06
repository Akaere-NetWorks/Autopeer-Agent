package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

var updateLog = logrus.WithField("pkg", "update")

var (
	backupDir     = "/var/lib/autopeer-agent/backup"
	downloadPart  = "/var/lib/autopeer-agent/autopeer-agent.part"
	downloadFinal = "/var/lib/autopeer-agent/autopeer-agent.new"
)

const (
	privateDirPerm  os.FileMode = 0700
	privateFilePerm os.FileMode = 0600
	privateExecPerm os.FileMode = 0700
	maxDownloadSize             = 50 * 1024 * 1024
)

func BackupPath(version string) string {
	return filepath.Join(backupDir, "autopeer-agent."+version)
}

func DownloadAndApply(url, expectedSHA256, newVersion, currentVersion, agentToken string) error {
	updateLog.Debugf("DownloadAndApply: url=%s newVersion=%s", url, newVersion)

	if err := ensurePrivateDir(filepath.Dir(backupDir)); err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}
	if err := ensurePrivateDir(backupDir); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}

	if err := downloadFile(url, expectedSHA256, agentToken); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	backupPath := BackupPath(currentVersion)
	updateLog.Debugf("DownloadAndApply: backup path=%s", backupPath)
	if err := copyFileAtomic(execPath, backupPath, privateFilePerm); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	if err := replaceFile(downloadFinal, execPath, privateExecPerm); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	updateLog.Debugf("DownloadAndApply: replace result=%s", execPath)
	updateLog.Infof("update applied successfully: newVersion=%s", newVersion)

	return nil
}

func RollbackFrom(backupPath string) error {
	updateLog.Debugf("RollbackFrom: entry backupPath=%s", backupPath)

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("backup not found at %s", backupPath)
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	if err := replaceFile(backupPath, execPath, privateExecPerm); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}

	updateLog.Debugf("RollbackFrom: restored %s -> %s", backupPath, execPath)
	updateLog.Info("rollback completed")

	return nil
}

func RestartSelf() {
	execPath, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}

	updateLog.Debugf("RestartSelf: executable=%s", execPath)

	if err := syscall.Exec(execPath, os.Args, os.Environ()); err != nil {
		updateLog.WithError(err).Error("RestartSelf: syscall.Exec failed, falling back to os.Exit(1)")
	}
	os.Exit(1)
}

func downloadFile(url, expectedSHA256, agentToken string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("refusing to download from non-HTTPS URL: %s", url)
	}

	if err := removeIfExists(downloadPart); err != nil {
		return fmt.Errorf("remove stale temp file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if agentToken != "" {
		req.Header.Set("X-Agent-Token", agentToken)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	f, err := os.OpenFile(downloadPart, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, privateFilePerm)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()

	hasher := sha256.New()
	writer := io.MultiWriter(f, hasher)

	if _, err := io.Copy(writer, io.LimitReader(resp.Body, maxDownloadSize+1)); err != nil {
		os.Remove(downloadPart)
		return fmt.Errorf("write: %w", err)
	}

	if info, err := f.Stat(); err == nil && info.Size() > maxDownloadSize {
		os.Remove(downloadPart)
		return fmt.Errorf("download exceeded maximum allowed size of %d bytes", maxDownloadSize)
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != expectedSHA256 {
		os.Remove(downloadPart)
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedSHA256, actual)
	}
	if err := f.Sync(); err != nil {
		os.Remove(downloadPart)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(downloadPart)
		return fmt.Errorf("close temp file: %w", err)
	}
	closed = true

	if err := os.Rename(downloadPart, downloadFinal); err != nil {
		return err
	}
	return syncDir(filepath.Dir(downloadFinal))
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	updateLog.Debugf("sha256File: path=%s hash=%s", path, hash)
	return hash, nil
}

func copyFileAtomic(src, dst string, perm os.FileMode) error {
	updateLog.Debugf("copyFileAtomic: src=%s dst=%s", src, dst)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dstDir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dstDir, "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, perm); err != nil {
		return err
	}
	return syncDir(dstDir)

}

func replaceFile(src, dst string, perm os.FileMode) error {
	if err := os.Chmod(src, perm); err != nil {
		return fmt.Errorf("set source permissions: %w", err)
	}
	if err := os.Rename(src, dst); err == nil {
		if err := os.Chmod(dst, perm); err != nil {
			return fmt.Errorf("set target permissions: %w", err)
		}
		return syncDir(filepath.Dir(dst))
	}

	if err := copyFileAtomic(src, dst, perm); err != nil {
		return err
	}
	return removeIfExists(src)
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, privateDirPerm)
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
