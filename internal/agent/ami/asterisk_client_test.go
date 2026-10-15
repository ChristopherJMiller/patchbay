//go:build asterisk

package ami_test

// The real-Asterisk check's clients. The Asterisk container has no network
// (and with rootless Docker the host could not reach it anyway), and AMI
// listens on its loopback only, so this test binary copies itself into the
// container and runs there, selected by checkModeEnv:
//
//   - "ami": the agent's AMI client (ami.Conn) logs in as AMI_USER and
//     AMI_PASS and runs the operations in AMI_OPS (JSON), printing one JSON
//     line per operation;
//   - "sip": a SIP user agent REGISTERs as a device (the SIP_* settings),
//     prints the REGISTER's outcome and Call-ID, then waits SIP_WAIT
//     seconds for one NOTIFY, answers it 200 and prints it.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/ChristopherJMiller/patchbay/internal/agent/actions"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// checkModeEnv selects a client mode inside the container.
const checkModeEnv = "PATCHBAY_AMI_CHECK"

// op is one AMI operation of the "ami" mode.
type op struct {
	Op     string `json:"op"`               // state, endpoints, contexts, contacts, channels, reload, resync, restart
	Arg    string `json:"arg,omitempty"`    // reload: the module; resync/restart: the device
	Millis int    `json:"millis,omitempty"` // sleep
}

// result is one operation's outcome.
type result struct {
	Op       string              `json:"op"`
	Arg      string              `json:"arg,omitempty"`
	State    *ami.State          `json:"state,omitempty"`
	Names    []string            `json:"names,omitempty"`
	Contacts []ami.Contact       `json:"contacts,omitempty"`
	Channels []ami.Channel       `json:"channels,omitempty"`
	Action   *api.ActionResponse `json:"action,omitempty"`
	Error    string              `json:"error,omitempty"`
}

// sipLine is what the "sip" mode prints.
type sipLine struct {
	Status  int                 `json:"status,omitempty"`
	CallID  string              `json:"callID,omitempty"`
	Contact string              `json:"contact,omitempty"`
	Notify  map[string][]string `json:"notify,omitempty"` // header -> its values, in order; "body"; "Request-URI"
	Error   string              `json:"error,omitempty"`
}

func runCheckClient(mode string) int {
	var err error
	switch mode {
	case "ami":
		err = runAMIClient()
	case "sip":
		err = runSIPClient()
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		fmt.Println(`{"error":` + strconv.Quote(err.Error()) + `}`)
		return 2
	}
	return 0
}

func runAMIClient() error {
	var ops []op
	if err := json.Unmarshal([]byte(os.Getenv("AMI_OPS")), &ops); err != nil {
		return err
	}
	var idx compile.Index
	if raw := os.Getenv("AMI_INDEX"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &idx); err != nil {
			return err
		}
	}
	c := ami.New(ami.Config{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(compile.AMIPort))})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	c.SetAccount(os.Getenv("AMI_USER"), os.Getenv("AMI_PASS"))
	deadline := time.Now().Add(15 * time.Second)
	for !c.State().Up && time.Now().Before(deadline) {
		if st := c.State(); strings.Contains(st.Error, "refused") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	acts := actions.New(actions.Config{
		AMI: c, Applied: func() (string, compile.Index, bool) { return idx.Version, idx, idx.Version != "" },
	})
	out := json.NewEncoder(os.Stdout)
	for _, o := range ops {
		r := result{Op: o.Op, Arg: o.Arg}
		cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
		var err error
		switch o.Op {
		case "state":
			st := c.State()
			r.State = &st
		case "endpoints":
			r.Names, err = c.Endpoints(cctx)
		case "contexts":
			r.Names, err = c.DialplanContexts(cctx)
		case "contacts":
			r.Contacts, err = c.Contacts(cctx)
		case "channels":
			r.Channels, err = c.Channels(cctx)
		case "reload":
			err = c.Reload(cctx, o.Arg)
		case "resync":
			resp := acts.Resync(cctx, api.ActionRequest{Device: o.Arg})
			r.Action = &resp
		case "restart":
			resp := acts.Restart(cctx, api.ActionRequest{Device: o.Arg})
			r.Action = &resp
		case "sleep":
			time.Sleep(time.Duration(o.Millis) * time.Millisecond)
		default:
			err = fmt.Errorf("unknown op %q", o.Op)
		}
		ccancel()
		if err != nil {
			r.Error = err.Error()
		}
		if err := out.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func runSIPClient() error {
	server, transport := os.Getenv("SIP_SERVER"), os.Getenv("SIP_TRANSPORT")
	user, authUser, password := os.Getenv("SIP_USER"), os.Getenv("SIP_AUTH_USER"), os.Getenv("SIP_PASSWORD")
	port, err1 := strconv.Atoi(os.Getenv("SIP_PORT"))
	wait, err2 := strconv.Atoi(os.Getenv("SIP_WAIT"))
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait+20)*time.Second)
	defer cancel()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("patchbay-ami-check"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		return err
	}
	defer func() { _ = ua.Close() }()
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return err
	}
	notifies := make(chan *sip.Request, 1)
	srv.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		select {
		case notifies <- req:
		default:
		}
	})
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"), sipgo.WithClientPort(port))
	if err != nil {
		return err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var lc net.ListenConfig
	udp, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = udp.Close() }()
	tcp, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = tcp.Close() }()
	go func() { _ = srv.ServeUDP(udp) }()
	go func() { _ = srv.ServeTCP(tcp) }()

	host, sport, _ := net.SplitHostPort(server)
	p, _ := strconv.Atoi(sport)
	recipient := sip.Uri{Scheme: "sip", Host: host, Port: p, UriParams: sip.HeaderParams{{K: "transport", V: transport}}}
	aor := sip.Uri{Scheme: "sip", User: user, Host: host}
	req := sip.NewRequest(sip.REGISTER, recipient)
	from := &sip.FromHeader{Address: aor, Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(16))
	exp := sip.ExpiresHeader(600)
	req.AppendHeader(from)
	req.AppendHeader(&sip.ToHeader{Address: aor})
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{
		Scheme: "sip", User: user, Host: "127.0.0.1", Port: port, UriParams: sip.HeaderParams{{K: "transport", V: transport}},
	}})
	req.AppendHeader(&exp)
	req.AppendHeader(sip.NewHeader("User-Agent", "patchbay-ami-check"))
	req.SetTransport(strings.ToUpper(transport))
	res, err := client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}
	if res.StatusCode == sip.StatusUnauthorized {
		if res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{Username: authUser, Password: password}); err != nil {
			return err
		}
	}
	line := sipLine{Status: res.StatusCode}
	if h := req.CallID(); h != nil {
		line.CallID = h.Value()
	}
	if h := res.Contact(); h != nil {
		line.Contact = h.Address.String()
	}
	out := json.NewEncoder(os.Stdout)
	if err := out.Encode(line); err != nil {
		return err
	}
	_ = bufio.NewWriter(os.Stdout).Flush()

	select {
	case n := <-notifies:
		got := map[string][]string{"body": {string(n.Body())}, "Request-URI": {n.Recipient.String()}}
		for _, h := range n.Headers() {
			got[h.Name()] = append(got[h.Name()], h.Value())
		}
		return out.Encode(sipLine{Notify: got})
	case <-time.After(time.Duration(wait) * time.Second):
		return out.Encode(sipLine{Error: "no NOTIFY within " + strconv.Itoa(wait) + " s"})
	}
}
