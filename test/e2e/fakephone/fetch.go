package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

const (
	// tftpTimeout is how long one TFTP packet may take (three tries each).
	tftpTimeout = 5 * time.Second
	// maxConfigSize caps a config document (the SEP file is ~10 KB).
	maxConfigSize = 1 << 20
	// provisioningSource is how State and the log name the provisioning URL,
	// which carries a one-time token and is never written anywhere.
	provisioningSource = "provisioning"
)

// tftpErrorPattern is pin/tftp's error for a TFTP ERROR packet.
var tftpErrorPattern = regexp.MustCompile(`code: (\d+), message: (.*)$`)

// retry repeats attempt every r.Every, for at most r.Within, until its
// result is expect (default Served; Any takes anything). An error from
// attempt (a served config that does not parse) ends it at once.
func (r *runner) retry(ctx context.Context, expect script.Result, rt script.Retry, attempt func() (script.Result, error)) error {
	if expect == "" {
		expect = script.Served
	}
	deadline := time.Now().Add(rt.Within.D())
	for {
		got, err := attempt()
		if err != nil {
			return err
		}
		if expect == script.Any || got == expect {
			return nil
		}
		if rt.Every <= 0 || time.Now().Add(rt.Every.D()).After(deadline) {
			return fmt.Errorf("the fetch was %s, want %s", got, expect)
		}
		if err := sleep(ctx, rt.Every.D()); err != nil {
			return err
		}
	}
}

// fetchConfig fetches the phone's config document once (cisco: TFTP
// SEP<MAC>.cnf.xml; linphone: GET the provisioning URL), records the
// fetch and, when it was served, parses it into the current config.
func (r *runner) fetchConfig(ctx context.Context) (script.Result, error) {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	var (
		doc []byte
		rec script.FetchRecord
	)
	if r.sc.Mode == script.ModeCisco {
		doc, rec = r.tftp("SEP"+r.sc.MAC+".cnf.xml", true)
	} else {
		doc, rec = r.getProvisioning(ctx)
	}
	r.recordFetch(rec)
	if rec.Result != script.Served {
		return rec.Result, nil
	}
	var (
		cfg *deviceConfig
		err error
	)
	if r.sc.Mode == script.ModeCisco {
		cfg, err = parseSEP(doc, rec.File)
	} else {
		cfg, err = parseLPConfig(doc)
	}
	if err != nil {
		return rec.Result, fmt.Errorf("the config was served but does not parse: %w", err)
	}
	r.setConfig(cfg)
	return rec.Result, nil
}

// fetchFile is a Fetch step: TFTP one file (its body is only counted and
// hashed), retried as the step says.
func (r *runner) fetchFile(ctx context.Context, f *script.Fetch) error {
	load := ""
	if cfg := r.currentConfig(); cfg != nil {
		load = cfg.Public.LoadInformation
	}
	if strings.Contains(f.File, "{load}") && load == "" {
		return fmt.Errorf("%s names {load}, and no config naming a load has been read", f.File)
	}
	file := script.Expand(f.File, r.sc.MAC, load)
	return r.retry(ctx, f.Expect, f.Retry, func() (script.Result, error) {
		_, rec := r.tftp(file, false)
		r.recordFetch(rec)
		return rec.Result, nil
	})
}

// tftp fetches file from the script's server in octet mode, as the phone
// does, and returns its body (when keep) and the record of the fetch.
func (r *runner) tftp(file string, keep bool) ([]byte, script.FetchRecord) {
	rec := script.FetchRecord{File: file, At: time.Now()}
	sum := sha256.New()
	var body bytes.Buffer
	w := io.Writer(sum)
	if keep {
		w = io.MultiWriter(sum, &limitedWriter{w: &body, n: maxConfigSize})
	}
	server := net.JoinHostPort(r.sc.Server, strconv.Itoa(r.opts.TFTPPort))
	n, err := tftpGet(server, file, r.opts.TFTPTimeout, w)
	if err != nil {
		rec.Result, rec.Error = classifyTFTP(err)
		return nil, rec
	}
	rec.Result, rec.Bytes, rec.SHA256 = script.Served, n, hex.EncodeToString(sum.Sum(nil))
	return body.Bytes(), rec
}

// classifyTFTP sorts a failed TFTP read: an ERROR packet with code 1 (File
// not found) is NotFound; anything else (another code, a timeout) Failed.
func classifyTFTP(err error) (script.Result, string) {
	if m := tftpErrorPattern.FindStringSubmatch(err.Error()); m != nil {
		msg := strings.TrimRight(m[2], "\x00 ") // the packet's terminating NUL
		if m[1] == "1" {
			return script.NotFound, msg
		}
		return script.Failed, "TFTP error " + m[1] + ": " + msg
	}
	return script.Failed, err.Error()
}

// getProvisioning GETs the provisioning URL once. The record names it
// "provisioning", and no error quotes the URL.
func (r *runner) getProvisioning(ctx context.Context) ([]byte, script.FetchRecord) {
	rec := script.FetchRecord{File: provisioningSource, At: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.sc.ProvisioningURL, nil)
	if err != nil {
		rec.Result, rec.Error = script.Failed, "the provisioning URL is not a valid URL"
		return nil, rec
	}
	req.Header.Set("User-Agent", r.userAgent())
	res, err := r.opts.HTTP.Do(req)
	if err != nil {
		rec.Result, rec.Error = script.Failed, unwrapURLError(err).Error()
		return nil, rec
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxConfigSize))
	if err != nil {
		rec.Result, rec.Error = script.Failed, unwrapURLError(err).Error()
		return nil, rec
	}
	switch res.StatusCode {
	case http.StatusOK:
		rec.Result, rec.Bytes, rec.SHA256 = script.Served, int64(len(body)), hex.EncodeToString(sha256Of(body))
		return body, rec
	case http.StatusNotFound:
		rec.Result, rec.Error = script.NotFound, "HTTP 404"
	default:
		rec.Result, rec.Error = script.Failed, "HTTP "+strconv.Itoa(res.StatusCode)
	}
	return nil, rec
}

func (r *runner) recordFetch(rec script.FetchRecord) {
	r.mu.Lock()
	r.state.Fetches = append(r.state.Fetches, rec)
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("fetch", "record", rec)
}

// setConfig makes cfg the phone's config: State shows it, and the phone
// calls as its account from now on.
func (r *runner) setConfig(cfg *deviceConfig) {
	r.mu.Lock()
	r.config = cfg
	pub := cfg.Public
	r.state.Config = &pub
	r.changedLocked()
	r.mu.Unlock()
	r.phone.setAccount(cfg.account())
	r.log.Info("config", "config", pub)
}

// limitedWriter keeps at most n bytes and fails past them.
type limitedWriter struct {
	w io.Writer
	n int64
}

var errTooLarge = errors.New("the config document is over 1 MiB")

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errTooLarge
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}
