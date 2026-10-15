package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// buildOffer is the SDP a 79x5 sends for a G.711 call: PCMU plus RFC 4733
// telephone-events on payload 101, 20 ms packets.
func buildOffer(ip netip.Addr, port int, sessionID uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=fakephone %d %d IN IP4 %s\r\n", sessionID, sessionID, ip)
	fmt.Fprintf(&b, "s=fakephone\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", ip)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio %d RTP/AVP %d 101\r\n", port, payloadTypePCMU)
	fmt.Fprintf(&b, "a=rtpmap:%d PCMU/%d\r\n", payloadTypePCMU, sampleRate)
	fmt.Fprintf(&b, "a=rtpmap:101 telephone-event/%d\r\n", sampleRate)
	fmt.Fprintf(&b, "a=fmtp:101 0-15\r\n")
	fmt.Fprintf(&b, "a=ptime:%d\r\n", frameDuration)
	fmt.Fprintf(&b, "a=sendrecv\r\n")
	return []byte(b.String())
}

// remoteMedia is what the answer says about where to send audio.
type remoteMedia struct {
	Addr         netip.AddrPort
	PayloadTypes []int
}

// parseAnswer reads the audio destination from an SDP answer: the media-level
// c= line if present, else the session-level one, and the m=audio port.
func parseAnswer(body []byte) (remoteMedia, error) {
	var (
		sessionAddr, mediaAddr netip.Addr
		port                   = -1 // -1: no m=audio line seen yet
		pts                    []int
		inAudio                bool
	)
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "m="):
			fields := strings.Fields(strings.TrimPrefix(line, "m="))
			inAudio = len(fields) >= 3 && fields[0] == "audio" && port < 0
			if !inAudio {
				continue
			}
			p, err := strconv.ParseUint(fields[1], 10, 16)
			if err != nil {
				return remoteMedia{}, fmt.Errorf("sdp: bad m=audio port %q", fields[1])
			}
			port = int(p)
			for _, f := range fields[3:] {
				pt, err := strconv.Atoi(f)
				if err != nil {
					return remoteMedia{}, fmt.Errorf("sdp: bad payload type %q", f)
				}
				pts = append(pts, pt)
			}
		case strings.HasPrefix(line, "c="):
			fields := strings.Fields(strings.TrimPrefix(line, "c="))
			if len(fields) != 3 || fields[0] != "IN" || fields[1] != "IP4" {
				return remoteMedia{}, fmt.Errorf("sdp: unsupported connection line %q", line)
			}
			a, err := netip.ParseAddr(fields[2])
			if err != nil {
				return remoteMedia{}, fmt.Errorf("sdp: bad connection address: %w", err)
			}
			if inAudio {
				mediaAddr = a
			} else if port < 0 {
				sessionAddr = a
			}
		}
	}
	if err := sc.Err(); err != nil {
		return remoteMedia{}, err
	}
	if port <= 0 {
		return remoteMedia{}, errors.New("sdp: no active m=audio line")
	}
	addr := mediaAddr
	if !addr.IsValid() {
		addr = sessionAddr
	}
	if !addr.IsValid() {
		return remoteMedia{}, errors.New("sdp: no connection address")
	}
	return remoteMedia{Addr: netip.AddrPortFrom(addr, uint16(port)), PayloadTypes: pts}, nil // port came from ParseUint(…, 16)
}
