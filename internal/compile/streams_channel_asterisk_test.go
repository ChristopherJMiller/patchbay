//go:build asterisk

package compile_test

// TestRealAsteriskStreamChannel (make asterisk-check): what the agent's AMI
// client sees while the desk phone listens to a stream, on the pinned Asterisk
// image with the stand-in stream bridge. The phone's channel must be counted
// as a call into a Stream (observe.StreamCalls), which is what lets the
// controller leave messages showing during radio and TV. This lives beside
// the streams check because it needs that check's AudioSocket stand-in and
// SIP caller; the test binary also runs in the container as an AMI client.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/observe"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// amiChannelsEnv selects the AMI client mode: it prints Channels as JSON.
const amiChannelsEnv = "PATCHBAY_CHECK_AMI_CHANNELS"

// runAMIChannels is the AMI client mode, inside the container.
func runAMIChannels() int {
	c := ami.New(ami.Config{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(compile.AMIPort))})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	c.SetAccount(compile.AMIUsername, os.Getenv("AMI_PASS"))
	for !c.State().Up {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "AMI never came up:", c.State().Error)
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}
	chs, err := c.Channels(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(chs); err != nil {
		return 1
	}
	return 0
}

func (c asteriskContainer) amiChannels(pass string) []ami.Channel {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := docker(ctx, "exec", "-e", amiChannelsEnv+"=1", "-e", "AMI_PASS="+pass, c.name, clientDir+"/client")
	if err != nil {
		c.t.Fatalf("AMI client: %v", err)
	}
	var chs []ami.Channel
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "[") {
			if err := json.Unmarshal([]byte(line), &chs); err != nil {
				c.t.Fatalf("AMI client line %q: %v", line, err)
			}
		}
	}
	return chs
}

func TestRealAsteriskStreamChannel(t *testing.T) {
	env := newCheckEnv(t)
	in, s := loadGolden(t, "streams")
	s = checkSettings(s, true)
	s.StreamBridge = checkBridge
	s.Streams.CallLimit = metav1.Duration{Duration: compile.MinCallLimit}
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	c := startAsterisk(t, env, fmt.Sprintf("patchbay-asterisk-check-%d-stream-channel", os.Getpid()), b, in.Secrets)
	c.audioSocket(map[string]string{
		"AUDIOSOCKET_LISTEN":      checkBridge,
		"AUDIOSOCKET_PLAY":        streams.UUID("komo4") + "=20s",
		"AUDIOSOCKET_CONNECTIONS": "1",
	})

	// The desk calls 704 (komo4) and stays up for 8 s, in the background.
	id := drivers.DeviceID(drivers.KindPhone, "desk")
	callEnv := streamCallEnv(t, b, s, in.Secrets, id, s.SIPPort+310, "704", 8*time.Second)
	called := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		args := []string{"exec", "-e", sipClientEnv + "=call"}
		for k, v := range callEnv {
			args = append(args, "-e", k+"="+v)
		}
		_, err := docker(ctx, append(args, c.name, clientDir+"/client")...)
		called <- err
	}()

	pass := string(in.Secrets[s.AMISecret])
	var chs []ami.Channel
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if chs = c.amiChannels(pass); observe.StreamCalls(chs)[id] > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("channels while the desk listens to 704: %+v", chs)
	if got := observe.StreamCalls(chs); got[id] != 1 || len(got) != 1 {
		t.Errorf("StreamCalls = %v; want the desk's one channel counted as a Stream call", got)
	}
	if err := <-called; err != nil {
		t.Errorf("the desk's call: %v", err)
	}
	if after := c.amiChannels(pass); len(after) != 0 {
		t.Logf("channels after the call (it may still be clearing): %+v", after)
	}
}
