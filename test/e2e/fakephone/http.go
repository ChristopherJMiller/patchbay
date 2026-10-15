package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// httpGet fetches url with the given "Name: value" headers and returns the
// status code and up to 1 MiB of the body. It is how a pod-network client
// (the controller, a softphone) sees the agent's HTTP endpoints.
func httpGet(ctx context.Context, url string, headers []string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, body, err
}
