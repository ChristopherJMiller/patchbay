package fileserver

import (
	"errors"
	"io"
	"time"

	"github.com/pin/tftp/v3"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// maxTransfers is how many TFTP transfers run at once. A phone fetches a
// handful of files at boot; the cap only stops a flood of read requests
// from the voice VLAN from holding a socket and a goroutine each.
const maxTransfers = 128

// The TFTP error messages a client sees: never a path, never why.
var (
	errTFTPNotFound = errors.New("file not found")
	errTFTPFailed   = errors.New("server error")
	errTFTPBusy     = errors.New("server busy")
)

// tftpOptions are the TFTP server's timing: 5 s per round trip and 5
// tries in production; tests shorten them.
type tftpOptions struct {
	timeout time.Duration
	retries int
	backoff func(attempt int) time.Duration // nil: pin/tftp's (up to 1 s)
	// transfers caps the transfers in flight. 0: maxTransfers.
	transfers int
}

var defaultTFTP = tftpOptions{timeout: 5 * time.Second, retries: 5}

// tftpServer serves read requests only: pin/tftp refuses a write request
// with an error packet, and nothing reports it. Replies come from a new
// socket per transfer, bound to the address the request came to
// (RequestPacketInfo.LocalIP, from IP_PKTINFO), so a host with several
// addresses on one interface answers from the PBX address.
func (s *server) tftpServer(o tftpOptions) *tftp.Server {
	ts := tftp.NewServer(s.tftpRead, nil)
	ts.SetTimeout(o.timeout)
	ts.SetRetries(o.retries)
	if o.backoff != nil {
		ts.SetBackoff(o.backoff)
	}
	return ts
}

// tftpRead serves one read request and reports it once: served when the
// last block was acknowledged, error when the transfer failed, notFound
// when nothing was sent.
func (s *server) tftpRead(filename string, rf io.ReaderFrom) error {
	source := ""
	if ot, ok := rf.(tftp.OutgoingTransfer); ok {
		addr := ot.RemoteAddr()
		source = canonicalIP(addr.IP)
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		ev := failed(sensor.FetchEvent{File: fileOf(filename)}, "tftp", errTFTPBusy)
		s.report(ev, api.TransportTFTP, source)
		return errTFTPBusy
	}

	f, ev := s.open(filename)
	if f == nil {
		s.report(ev, api.TransportTFTP, source)
		if ev.Result == api.FetchNotFound {
			return errTFTPNotFound
		}
		return errTFTPFailed
	}
	defer f.Close()
	if _, err := rf.ReadFrom(f); err != nil {
		// ReadFrom has already sent the client an error packet.
		ev = failed(ev, "tftp", err)
		s.report(ev, api.TransportTFTP, source)
		return nil
	}
	ev.Result = api.FetchServed
	s.report(ev, api.TransportTFTP, source)
	return nil
}
