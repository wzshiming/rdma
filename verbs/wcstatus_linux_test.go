//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import "testing"

// The Go table behind WCStatus.String must carry the texts of ibv_wc_status_str.
func TestWCStatusStringsMatchLibibverbs(t *testing.T) {
	for s := WCSuccess; s <= WCTMRndvIncomplete; s++ {
		want, err := wcStatusStr(s)
		if err != nil {
			t.Fatalf("ibv_wc_status_str(%d): %v", s, err)
		}
		if got := s.String(); got != want {
			t.Errorf("WCStatus(%d).String() = %q, ibv_wc_status_str = %q", s, got, want)
		}
	}
}
