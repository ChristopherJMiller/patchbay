package main

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestOfferRoundTrip(t *testing.T) {
	ip := netip.MustParseAddr("10.244.0.7")
	offer := buildOffer(ip, 41000, 42)
	for _, want := range []string{
		"c=IN IP4 10.244.0.7\r\n",
		"m=audio 41000 RTP/AVP 0 101\r\n",
		"a=rtpmap:0 PCMU/8000\r\n",
		"a=rtpmap:101 telephone-event/8000\r\n",
		"a=ptime:20\r\n",
	} {
		if !strings.Contains(string(offer), want) {
			t.Errorf("offer lacks %q:\n%s", want, offer)
		}
	}
	got, err := parseAnswer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != netip.MustParseAddrPort("10.244.0.7:41000") || !slices.Equal(got.PayloadTypes, []int{0, 101}) {
		t.Fatalf("parsed %+v", got)
	}
}

// An answer in the shape Asterisk 22 sends (session-level c=).
const asteriskAnswer = "v=0\r\n" +
	"o=- 1 3 IN IP4 172.18.0.2\r\n" +
	"s=Asterisk\r\n" +
	"c=IN IP4 172.18.0.2\r\n" +
	"t=0 0\r\n" +
	"m=audio 10018 RTP/AVP 0 101\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:101 telephone-event/8000\r\n" +
	"a=fmtp:101 0-16\r\n" +
	"a=ptime:20\r\n" +
	"a=maxptime:150\r\n" +
	"a=sendrecv\r\n"

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		name, sdp string
		want      string // addr:port, or "" for an error
	}{
		{"asterisk", asteriskAnswer, "172.18.0.2:10018"},
		{"media-level c= wins", "v=0\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\nc=IN IP4 192.0.2.2\r\n", "192.0.2.2:5004"},
		{"first audio stream only", "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 5004 RTP/AVP 0\r\nm=audio 6004 RTP/AVP 0\r\nc=IN IP4 192.0.2.9\r\n", "192.0.2.1:5004"},
		{"video is ignored", "v=0\r\nc=IN IP4 192.0.2.1\r\nm=video 7000 RTP/AVP 96\r\nm=audio 5004 RTP/AVP 0\r\n", "192.0.2.1:5004"},
		{"rejected stream", "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 0 RTP/AVP 0\r\n", ""},
		{"no audio", "v=0\r\nc=IN IP4 192.0.2.1\r\n", ""},
		{"no address", "v=0\r\nm=audio 5004 RTP/AVP 0\r\n", ""},
		{"IPv6 unsupported", "v=0\r\nc=IN IP6 ::1\r\nm=audio 5004 RTP/AVP 0\r\n", ""},
		{"bad port", "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 99999 RTP/AVP 0\r\n", ""},
	}
	for _, c := range cases {
		got, err := parseAnswer([]byte(c.sdp))
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: parsed %v, want an error", c.name, got.Addr)
		case c.want != "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && got.Addr.String() != c.want:
			t.Errorf("%s: got %v, want %s", c.name, got.Addr, c.want)
		}
	}
}
