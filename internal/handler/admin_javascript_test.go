package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAdminJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	pages := map[string]string{
		"chat":      adminClientJS() + modelsPageJS(),
		"audio":     adminClientJS() + audioPageJS(),
		"documents": adminClientJS() + documentsPageJS(),
	}
	for name, script := range pages {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".js")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
				t.Fatalf("invalid JavaScript: %v\n%s", err, output)
			}
		})
	}
}
