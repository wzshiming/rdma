//go:build linux && cgo

package verbs

import "testing"

// The Go table behind WCStatus.String must carry the texts of ibv_wc_status_str.
func TestWCStatusStringsMatchLibibverbs(t *testing.T) {
	for s := WCSuccess; s <= WCTMRndvIncomplete; s++ {
		if got, want := s.String(), wcStatusStr(s); got != want {
			t.Errorf("WCStatus(%d).String() = %q, ibv_wc_status_str = %q", s, got, want)
		}
	}
}
