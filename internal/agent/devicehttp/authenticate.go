package devicehttp

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The phone's authenticationURL answers (usecallmanager.nz/phone-services.html):
// exactly these bodies, no newline.
const (
	authorizedBody   = "AUTHORIZED"
	unauthorizedBody = "UN-AUTHORIZED"

	// maxAuthBody bounds the form body read from the phone.
	maxAuthBody = 4 << 10
)

// authenticate answers the phone's question "are these the credentials of
// the /CGI/Execute you just got?". Which method and parameter names the
// phone really uses is unconfirmed (Cisco's SDK: GET with UserID, Password,
// devicename), so it accepts a GET query, a POST form and an Authorization:
// Basic header, names in any case, and logs the method and the parameter
// names it saw (never a value) so the bench can learn them. Nothing here is
// a fetch event.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	vals := url.Values{}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			vals = r.Form // query and body
		} else {
			vals = r.URL.Query()
		}
	} else {
		vals = r.URL.Query()
	}
	names := make([]string, 0, len(vals)+1)
	get := func(want string) string {
		for k, v := range vals {
			if strings.EqualFold(k, want) && len(v) > 0 {
				return v[0]
			}
		}
		return ""
	}
	for k := range vals {
		names = append(names, k)
	}
	slices.Sort(names)
	user, pass := get("UserID"), get("Password")
	if u, p, ok := r.BasicAuth(); ok {
		names = append(names, "Authorization: Basic")
		if user == "" && pass == "" {
			user, pass = u, p
		}
	}

	good := false
	if s.credentials != nil {
		if wu, wp, ok := s.credentials(); ok && wu != "" && wp != "" {
			uOK := equalDigest(user, wu)
			pOK := equalDigest(pass, wp)
			good = uOK&pOK == 1
		}
	}
	body, result := unauthorizedBody, unauthorizedBody
	if good {
		body, result = authorizedBody, authorizedBody
	}
	s.log.Info("device HTTP authenticate", "method", r.Method, "params", strings.Join(names, ","), "result", result)
	writeText(w, http.StatusOK, body, r.Method == http.MethodHead)
}

// equalDigest is 1 when a and b are equal, compared as SHA-256 digests in
// constant time (as match does), so not even the length leaks.
func equalDigest(a, b string) int {
	da, db := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:])
}
