package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gate admits exactly one thing, a POST the browser marks same-origin;
// every other method and every other Sec-Fetch-Site value, including a
// missing header (curl, an older browser), is refused.
func TestSameOriginPOST(t *testing.T) {
	cases := []struct {
		name   string
		method string
		site   string
		want   bool
	}{
		{name: "same-origin post", method: http.MethodPost, site: "same-origin", want: true},
		{name: "cross-site post", method: http.MethodPost, site: "cross-site", want: false},
		{name: "same-site post", method: http.MethodPost, site: "same-site", want: false},
		{name: "none post", method: http.MethodPost, site: "none", want: false},
		{name: "no header", method: http.MethodPost, site: "", want: false},
		{name: "same-origin get", method: http.MethodGet, site: "same-origin", want: false},
		{name: "same-origin put", method: http.MethodPut, site: "same-origin", want: false},
		{name: "case is exact", method: http.MethodPost, site: "Same-Origin", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/v1/admin/derive/skill-invocations/rerun", nil)
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if got := SameOriginPOST(r); got != tc.want {
				t.Errorf("SameOriginPOST(%s, %q) = %v, want %v", tc.method, tc.site, got, tc.want)
			}
		})
	}
}
