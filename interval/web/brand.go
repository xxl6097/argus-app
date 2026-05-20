// brand.go — server-side substitution of dashboard / login HTML brand
// strings.
//
// dashboardHTML and loginHTML are embedded at build time with the
// default 「WiFi 考勤」/「工时统计」 strings. We let users override these
// from the settings modal, so on every / and /login GET we string-
// replace the defaults with the configured pair before writing out.
//
// The ETag is rebuilt to fold in the brand signature so the browser
// re-fetches when the user changes the title — without that, the
// content-only ETag would still match and the old strings would stick
// around after the API write.

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/xxl6097/argus-app/interval/store/settings"
)

// brandedHTML returns the body to write for a dashboard / login GET,
// plus the ETag the response should advertise. baseETag is the static
// per-binary ETag computed from the embedded bytes; the returned ETag
// folds in the current brand signature so it changes on every brand
// edit.
//
// When settings is unattached or the brand pair equals the defaults,
// the response is the embedded bytes verbatim with the baseETag.
func (s *Server) brandedHTML(htmlBytes []byte, baseETag string) ([]byte, string) {
	if s.settings == nil {
		return htmlBytes, baseETag
	}
	title, subtitle := s.settings.Brand()
	if title == settings.DefaultBrandTitle && subtitle == settings.DefaultBrandSubtitle {
		return htmlBytes, baseETag
	}
	body := string(htmlBytes)
	// Replace subtitle first: "工时统计" never overlaps "WiFi 考勤".
	if subtitle != settings.DefaultBrandSubtitle {
		body = strings.ReplaceAll(body, settings.DefaultBrandSubtitle, subtitle)
	}
	if title != settings.DefaultBrandTitle {
		body = strings.ReplaceAll(body, settings.DefaultBrandTitle, title)
	}
	return []byte(body), brandETag(baseETag, title, subtitle)
}

// brandETag derives a stable ETag from baseETag plus the brand pair.
// Same inputs always produce the same ETag, so a brand-unchanged GET
// still hits the 304 path.
func brandETag(baseETag, title, subtitle string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(baseETag))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(title))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(subtitle))
	sum := h.Sum(nil)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}
