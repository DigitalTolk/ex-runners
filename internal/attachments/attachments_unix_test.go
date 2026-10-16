//go:build !windows

package attachments

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadUnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	file := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(file, []byte("x"), 0o000)
	if _, err := Upload(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "tok", file); err == nil || !os.IsPermission(err) {
		t.Errorf("err = %v", err)
	}
}
