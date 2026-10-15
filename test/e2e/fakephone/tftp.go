package main

import (
	"fmt"
	"io"
	"time"

	"github.com/pin/tftp/v3"
)

// tftpGet fetches file from server (host:port) in octet mode, the way a
// 79x5 fetches SEP<MAC>.cnf.xml, and writes it to w.
func tftpGet(server, file string, timeout time.Duration, w io.Writer) (int64, error) {
	c, err := tftp.NewClient(server)
	if err != nil {
		return 0, fmt.Errorf("tftp: %w", err)
	}
	c.SetTimeout(timeout)
	c.SetRetries(3)
	wt, err := c.Receive(file, "octet")
	if err != nil {
		return 0, fmt.Errorf("tftp get %s from %s: %w", file, server, err)
	}
	n, err := wt.WriteTo(w)
	if err != nil {
		return n, fmt.Errorf("tftp get %s from %s: %w", file, server, err)
	}
	return n, nil
}
