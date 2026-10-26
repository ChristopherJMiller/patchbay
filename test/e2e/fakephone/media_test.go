package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp/v2"
)

// peer answers RTP from 127.0.0.1: echo sends every packet back unchanged,
// otherwise it answers each with a packet of µ-law silence.
func peer(t *testing.T, echo bool) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			out := buf[:n]
			if !echo {
				var pkt rtp.Packet
				if pkt.Unmarshal(out) != nil {
					continue
				}
				for i := range pkt.Payload {
					pkt.Payload[i] = 0xFF
				}
				if out, err = pkt.Marshal(); err != nil {
					continue
				}
			}
			_, _ = c.WriteToUDPAddrPort(out, from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func localRTP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var shortCall = mediaOptions{Duration: 400 * time.Millisecond, ToneHz: 1000, Tail: 200 * time.Millisecond}

func TestMediaEcho(t *testing.T) {
	res, err := runMedia(context.Background(), localRTP(t), peer(t, true), shortCall)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 20 {
		t.Errorf("sent %d packets in 400 ms, want 20", res.Sent)
	}
	// Loopback UDP can drop under load, but not most of a call.
	if res.ToneFrames < res.Sent*3/4 || res.Received != res.PCMU {
		t.Errorf("echo: %s", res)
	}
}

func TestMediaSilenceIsNotATone(t *testing.T) {
	res, err := runMedia(context.Background(), localRTP(t), peer(t, false), shortCall)
	if err != nil {
		t.Fatal(err)
	}
	if res.Received < res.Sent*3/4 {
		t.Errorf("silence peer: only %d of %d packets came back", res.Received, res.Sent)
	}
	if res.ToneFrames != 0 {
		t.Errorf("silence was taken for a tone: %s", res)
	}
}

func TestMediaWrongToneIsNotDetected(t *testing.T) {
	opts := shortCall
	opts.ToneHz = 400 // sent and looked for at 400 Hz...
	res, err := runMedia(context.Background(), localRTP(t), peer(t, true), opts)
	if err != nil || res.ToneFrames == 0 {
		t.Fatalf("400 Hz echo: %s (err %v)", res, err)
	}
	// ...and a 400 Hz echo carries no 1 kHz energy.
	g := &toneGenerator{freq: 400, amplitude: 0.5}
	if a := analyseULaw(g.nextFrame(), 1000); a.toneDetected() {
		t.Fatalf("400 Hz frame detected as 1 kHz: ratio %.3f", a.Ratio)
	}
}

func TestMediaHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	opts := shortCall
	opts.Duration = 10 * time.Second
	start := time.Now()
	_, err := runMedia(ctx, localRTP(t), peer(t, true), opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("runMedia took %s after its context ended", time.Since(start))
	}
}
