package web_test

import (
	"io/fs"
	"testing"

	"github.com/mchmarny/aicrme/internal/web"
)

func TestStaticServesIndex(t *testing.T) {
	static, err := web.Static()
	if err != nil {
		t.Fatalf("Static() error = %v", err)
	}
	if _, err := fs.Stat(static, "index.html"); err != nil {
		t.Errorf("index.html not embedded: %v", err)
	}
}
