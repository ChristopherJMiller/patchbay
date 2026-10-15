package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// httpDo sends method to url with the given "Name: value" headers and body
// (nil for none) and returns the status code and up to 1 MiB of the
// response body. It is how a pod-network client (the controller, a
// softphone) sees the agent's HTTP endpoints.
func httpDo(ctx context.Context, method, url string, headers []string, body []byte) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return 0, nil, err
	}
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return 0, nil, fmt.Errorf("header %q is not \"Name: value\"", h)
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, redactURL(url), unwrapURLError(err))
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, b, err
}

// unwrapURLError drops net/http's *url.Error wrapper, which repeats the
// URL (and so a provisioning token) in its message.
func unwrapURLError(err error) error {
	if ue, ok := err.(*url.Error); ok { //nolint:errorlint // only the outermost wrapper carries the URL.
		return ue.Err
	}
	return err
}

// provisioningPrefix is device HTTP's one-time provisioning path
// (agentapi.ProvisioningPathPrefix; fakephone imports nothing of patchbay's).
const provisioningPrefix = "/p/"

// redactURL hides a provisioning token in a URL, so it never reaches the
// test logs: http://host:8088/p/<token> becomes .../p/<redacted>.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable URL>"
	}
	if strings.HasPrefix(u.Path, provisioningPrefix) && len(u.Path) > len(provisioningPrefix) {
		u.Path = provisioningPrefix + "<redacted>"
		u.RawPath = ""
	}
	return u.String()
}
