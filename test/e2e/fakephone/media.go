package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// mediaOptions configures one call's audio.
type mediaOptions struct {
	// Duration is how long the tone is sent.
	Duration time.Duration
	// ToneHz is the frequency sent and looked for.
	ToneHz float64
	// Tail is how long to keep listening after the last packet is sent, for
	// echoed audio still in flight.
	Tail time.Duration
}

// mediaResult is what one call's audio did.
type mediaResult struct {
	Sent       int     // RTP packets sent
	Received   int     // RTP packets received (any payload type)
	PCMU       int     // received packets carrying PCMU
	ToneFrames int     // received PCMU packets in which the tone was detected
	MaxRatio   float64 // best tone ratio seen in one packet
	Source     string  // where the received audio came from
	// FirstTone is how long after the first packet was sent the tone first
	// came back: the echo path's latency. Zero if it never did.
	FirstTone time.Duration
}

func (r mediaResult) String() string {
	return fmt.Sprintf("sent=%d received=%d pcmu=%d toneFrames=%d maxRatio=%.3f firstTone=%s source=%s",
		r.Sent, r.Received, r.PCMU, r.ToneFrames, r.MaxRatio, r.FirstTone, r.Source)
}

// runMedia sends a PCMU tone from conn to remote for opts.Duration, paced
// every 20 ms, while measuring the tone in whatever comes back. It returns
// when the tone and the tail are done, or ctx ends; either way the receiver
// has stopped by then.
func runMedia(ctx context.Context, conn *net.UDPConn, remote netip.AddrPort, opts mediaOptions) (mediaResult, error) {
	var res mediaResult
	frames := int(opts.Duration / (frameDuration * time.Millisecond))
	if frames <= 0 {
		return res, errors.New("media: duration is shorter than one frame")
	}
	ssrc, seq, ts, err := rtpStart()
	if err != nil {
		return res, err
	}

	start := time.Now()
	recvDone := make(chan error, 1)
	var mu sync.Mutex // guards res's receive fields
	go func() {
		recvDone <- receive(conn, opts.ToneHz, start, &mu, &res)
	}()

	sendErr := func() error {
		gen := &toneGenerator{freq: opts.ToneHz, amplitude: 0.5}
		ticker := time.NewTicker(frameDuration * time.Millisecond)
		defer ticker.Stop()
		dst := net.UDPAddrFromAddrPort(remote)
		buf := make([]byte, 12+samplesPerFrame)
		for i := range frames {
			pkt := rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Marker:         i == 0,
					PayloadType:    payloadTypePCMU,
					SequenceNumber: seq,
					Timestamp:      ts,
					SSRC:           ssrc,
				},
				Payload: gen.nextFrame(),
			}
			n, err := pkt.MarshalTo(buf)
			if err != nil {
				return fmt.Errorf("media: marshal RTP: %w", err)
			}
			if _, err := conn.WriteToUDP(buf[:n], dst); err != nil {
				return fmt.Errorf("media: send RTP: %w", err)
			}
			res.Sent++
			seq++
			ts += samplesPerFrame
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
		// Listen a little longer for echoed audio still in flight.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Tail):
			return nil
		}
	}()

	// Stop the receiver by expiring its read deadline, and wait for it.
	_ = conn.SetReadDeadline(time.Now())
	recvErr := <-recvDone
	mu.Lock()
	defer mu.Unlock()
	return res, errors.Join(sendErr, recvErr)
}

// receive reads RTP until the connection's read deadline passes.
func receive(conn *net.UDPConn, toneHz float64, start time.Time, mu *sync.Mutex, res *mediaResult) error {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("media: receive RTP: %w", err)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue // not RTP (e.g. a stray STUN probe)
		}
		mu.Lock()
		res.Received++
		res.Source = from.String()
		if pkt.PayloadType == payloadTypePCMU {
			res.PCMU++
			a := analyseULaw(pkt.Payload, toneHz)
			res.MaxRatio = max(res.MaxRatio, a.Ratio)
			if a.toneDetected() {
				res.ToneFrames++
				if res.FirstTone == 0 {
					res.FirstTone = time.Since(start)
				}
			}
		}
		mu.Unlock()
	}
}

// rtpStart picks a random SSRC, sequence number and timestamp (RFC 3550 §5.1).
func rtpStart() (ssrc uint32, seq uint16, ts uint32, err error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, 0, 0, err
	}
	return binary.BigEndian.Uint32(b[0:4]), binary.BigEndian.Uint16(b[4:6]), binary.BigEndian.Uint32(b[6:10]), nil
}
