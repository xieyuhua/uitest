package jsruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeysJSMirrorOfExtension 保证 runtime 与 extension 两份 uitest-keys.js 不漂移。
//
// 这两份必须完全一致：Go 执行器 go:embed 的是 runtime/ 这份，浏览器插件加载的是
// extension/ 这份；一旦单边手改，录制时算出的 data-uitest-id 与执行时算的就不一样了。
func TestKeysJSMirrorOfExtension(t *testing.T) {
	const rel = "../extension/uitest-keys.js"
	raw, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Skipf("跳过：没有 %s（只签出服务端时正常）: %v", rel, err)
	}
	if got, want := strings.ReplaceAll(KeysJS, "\r\n", "\n"), strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Fatalf("runtime/uitest-keys.js 与 extension/uitest-keys.js 不一致，请把改过的那份复制到另一边")
	}
}

func TestVersionParsed(t *testing.T) {
	if strings.TrimSpace(Version) == "" {
		t.Fatal("没能从 uitest-keys.js 里解析出 VERSION")
	}
}
