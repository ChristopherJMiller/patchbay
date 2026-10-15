//go:build asterisk

package ami_test

// The real-Asterisk check of the agent's AMI client (make asterisk-check):
// the pinned Asterisk image, booted the way the PBX pod runs it on the
// compiled house bundle, driven by ami.Conn and the actions package. It
// settles what the fakes assume:
//
//   - the compiled manager.conf account logs in, and a wrong password is
//     refused without the password in any error;
//   - PJSIPShowEndpoints and ShowDialPlan give the bundle's endpoints and
//     contexts, and an empty endpoint or contact list is Asterisk's Error,
//     which the client reads as empty;
//   - PJSIPShowContacts after a REGISTER reports its Call-ID (CallId), the
//     open question of docs/plans/release-a.md W2.5;
//   - a resync (PJSIPNotify with the service-control body) reaches the
//     registered client exactly as sent;
//   - after the live directory is swapped, reloading res_pjsip.so and
//     pbx_config.so loads the new version, and every module apply reloads
//     (res_rtp_asterisk.so, res_pjsip_notify.so, indications, logger,
//     manager) reloads;
//   - a new AMI password takes effect when manager reloads over the old
//     session, as apply does it.
//
// Run it with:
//
//	CGO_ENABLED=0 nix develop -c go test -tags asterisk -count=1 -run '^TestRealAsterisk' -v ./internal/agent/ami/

import (
	"bufio"
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

const (
	// checkLabel is make asterisk-check's container label: its value is the
	// owning test process's PID, so asterisk-check-orphans can clean up.
	checkLabel = "patchbay.chrismiller.xyz/asterisk-check"
	// clientDir is where the container sees this test binary.
	clientDir   = "/opt/patchbay-check"
	bootTimeout = 90 * time.Second
)

var asteriskConf = compile.AsteriskConfigDir + "/asterisk.conf"

func TestMain(m *testing.M) {
	if mode := os.Getenv(checkModeEnv); mode != "" {
		os.Exit(runCheckClient(mode))
	}
	os.Exit(m.Run())
}

// chartAsteriskImage is the Asterisk image the chart pins.
func chartAsteriskImage(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "patchbay", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		PBX struct {
			Asterisk struct {
				Image struct{ Repository, Tag, Digest string } `json:"image"`
			} `json:"asterisk"`
		} `json:"pbx"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	img := values.PBX.Asterisk.Image
	if img.Repository == "" || img.Tag == "" || !strings.HasPrefix(img.Digest, "sha256:") {
		t.Fatalf("the chart pins no Asterisk image by digest: %+v", img)
	}
	return img.Repository + ":" + img.Tag + "@" + img.Digest
}

func docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// box is one running Asterisk and its live directory.
type box struct {
	t    *testing.T
	name string
	live string
}

// writeVersion lays out v<version>/asterisk/ world-readable (with rootless
// Docker the container's uid 1000 is not the files' owner; the values are
// the test's fake secrets) and points current at it.
func (b *box) writeVersion(r intake.Resolved) {
	b.t.Helper()
	dir, err := layout.VersionDir(r.Bundle.Version)
	if err != nil {
		b.t.Fatal(err)
	}
	for p, content := range r.Files {
		if !strings.HasPrefix(p, compile.TreeAsterisk+"/") {
			continue
		}
		dst := filepath.Join(b.live, dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			b.t.Fatal(err)
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
	for _, d := range []string{b.live, filepath.Join(b.live, dir), filepath.Join(b.live, dir, compile.TreeAsterisk)} {
		if err := os.Chmod(d, 0o755); err != nil {
			b.t.Fatal(err)
		}
	}
	tmp := filepath.Join(b.live, ".current-tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(dir, tmp); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(b.live, compile.CurrentLink)); err != nil {
		b.t.Fatal(err)
	}
}

// start runs the chart's Asterisk on the live directory, as the PBX pod
// does: uid 1000, read-only root, no capabilities, no network but its own
// loopback.
func (b *box) start(image, client string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmpfs := "uid=1000,gid=1000,mode=0750"
	if _, err := docker(ctx, "run", "--detach", "--name", b.name, "--label", checkLabel+"="+strconv.Itoa(os.Getpid()),
		"--network", "none",
		"--user", "1000:1000", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/var/run/asterisk:"+tmpfs, "--tmpfs", "/var/spool/asterisk:"+tmpfs, "--tmpfs", "/var/log/asterisk:"+tmpfs,
		"--mount", "type=bind,source="+b.live+",target="+compile.LiveDir+",readonly",
		"--mount", "type=bind,source="+client+",target="+clientDir+",readonly",
		"--entrypoint", "/usr/sbin/asterisk",
		image, "-f", "-n", "-vvv", "-C", asteriskConf,
	); err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if b.t.Failed() {
			logs, _ := docker(ctx, "logs", b.name)
			b.t.Logf("asterisk log of %s:\n%s", b.name, logs)
		}
		if out, err := docker(ctx, "rm", "--force", b.name); err != nil {
			b.t.Errorf("removing %s: %v\n%s", b.name, err, out)
		}
	})
	deadline := time.Now().Add(bootTimeout)
	for {
		out, err := docker(ctx, "exec", b.name, "/usr/sbin/asterisk", "-C", asteriskConf, "-rx", "core waitfullybooted")
		if err == nil && strings.Contains(out, "fully booted") {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("Asterisk is not fully booted after %s: %v", bootTimeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ami runs the agent's AMI client in the container as user/pass, with
// idx as the actions' applied index.
func (b *box) ami(user, pass string, idx compile.Index, ops ...op) []result {
	b.t.Helper()
	opsJSON, _ := json.Marshal(ops)
	idxJSON, _ := json.Marshal(idx)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := docker(ctx, "exec", "-e", checkModeEnv+"=ami", "-e", "AMI_USER="+user, "-e", "AMI_PASS="+pass,
		"-e", "AMI_OPS="+string(opsJSON), "-e", "AMI_INDEX="+string(idxJSON), b.name, clientDir+"/client")
	if err != nil {
		b.t.Fatalf("AMI client: %v", err)
	}
	var results []result
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue // what asterisk prints on a host without Ethernet, etc.
		}
		var r result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			b.t.Fatalf("AMI client line %q: %v", line, err)
		}
		results = append(results, r)
	}
	if len(results) != len(ops) {
		b.t.Fatalf("AMI client gave %d results for %d operations:\n%s", len(results), len(ops), out)
	}
	return results
}

// sipClient starts the SIP client registering as a device; its lines
// arrive on the returned channel.
func (b *box) sipClient(env map[string]string) <-chan sipLine {
	b.t.Helper()
	args := []string{"exec", "-e", checkModeEnv + "=sip"}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	cmd := exec.Command("docker", append(args, b.name, clientDir+"/client")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		b.t.Fatal(err)
	}
	lines := make(chan sipLine, 4)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var l sipLine
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				lines <- l
			}
		}
	}()
	b.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return lines
}

func nextLine(t *testing.T, lines <-chan sipLine) sipLine {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("the SIP client ended")
		}
		return l
	case <-time.After(90 * time.Second):
		t.Fatal("no line from the SIP client")
	}
	return sipLine{}
}

// newBox copies this static test binary where the container can run it.
func newBox(t *testing.T) (*box, string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatal("the test binary is dynamically linked: build it with CGO_ENABLED=0 (make asterisk-check does)")
		}
	}
	_ = f.Close()
	client := t.TempDir()
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(client, "client"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(client, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(t.TempDir(), "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &box{t: t, name: fmt.Sprintf("patchbay-asterisk-check-%d-ami", os.Getpid()), live: live}
	return b, chartAsteriskImage(t), client
}

// house is the house bundle adapted to the container: Asterisk binds
// 127.0.0.1, which the ACLs admit.
func house() *intaketest.Install {
	in := intaketest.House()
	in.Settings.PBXAddress = "127.0.0.1"
	in.Settings.AllowedNetworks = append(in.Settings.AllowedNetworks, "127.0.0.0/8")
	return in
}

func TestRealAsteriskAMI(t *testing.T) {
	b, image, client := newBox(t)
	in := house()
	r1 := in.Resolved(t)
	b.writeVersion(r1)
	b.start(image, client)
	desk := drivers.DeviceID(drivers.KindPhone, "desk")
	laptop := drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
	front := drivers.DeviceID(drivers.KindPhone, "front")
	user, pass := r1.Bundle.AMI.Username, r1.AMIPassword
	idx := r1.Bundle.Index()

	// Login, version, endpoints, contexts; nothing registered, no calls.
	res := b.ami(user, pass, idx, op{Op: "state"}, op{Op: "endpoints"}, op{Op: "contexts"}, op{Op: "contacts"}, op{Op: "channels"})
	if st := res[0].State; st == nil || !st.Up || !strings.HasPrefix(st.Version, "22.") {
		t.Fatalf("state %+v", res[0])
	}
	if !slices.Equal(res[1].Names, []string{desk, laptop}) || res[1].Error != "" {
		t.Errorf("endpoints %+v", res[1])
	}
	if !slices.Contains(res[2].Names, compile.DevicesContext) || !slices.Contains(res[2].Names, compile.UnavailableContext) || res[2].Error != "" {
		t.Errorf("contexts %+v", res[2])
	}
	if len(res[3].Contacts) != 0 || res[3].Error != "" {
		t.Errorf("contacts with nobody registered: %+v (Asterisk answers an Error; the client reads it as none)", res[3])
	}
	if len(res[4].Channels) != 0 || res[4].Error != "" {
		t.Errorf("channels %+v", res[4])
	}
	t.Logf("Asterisk %s; contexts %v", res[0].State.Version, res[2].Names)

	// A wrong password is refused, and never shown.
	res = b.ami(user, "not-the-password-"+pass, idx, op{Op: "state"})
	if st := res[0].State; st.Up || !strings.Contains(st.Error, "Authentication failed") || strings.Contains(st.Error, pass) {
		t.Errorf("wrong password: %+v", st)
	}

	// The desk registers (TCP, as its endpoint says): its contact carries
	// the REGISTER's Call-ID.
	sip := b.sipClient(map[string]string{
		"SIP_SERVER": "127.0.0.1:5060", "SIP_TRANSPORT": "tcp", "SIP_USER": "100", "SIP_AUTH_USER": desk,
		"SIP_PASSWORD": string(in.Secrets[compile.PasswordRef(desk)]), "SIP_PORT": "5170", "SIP_WAIT": "60",
	})
	reg := nextLine(t, sip)
	if reg.Status != 200 || reg.CallID == "" {
		t.Fatalf("REGISTER: %+v", reg)
	}
	res = b.ami(user, pass, idx, op{Op: "contacts"})
	if len(res[0].Contacts) != 1 {
		t.Fatalf("contacts after the REGISTER: %+v", res[0])
	}
	c := res[0].Contacts[0]
	t.Logf("PJSIPShowContacts after a REGISTER: %+v", c)
	if c.CallID != reg.CallID {
		t.Errorf("contact Call-ID %q, want the REGISTER's %q", c.CallID, reg.CallID)
	}
	if c.Endpoint != desk || c.AOR != "100" || c.Address != "127.0.0.1" ||
		!strings.HasPrefix(c.URI, "sip:100@127.0.0.1:5170") || c.Qualify != api.QualifyUnknown {
		t.Errorf("contact %+v", c)
	}
	if c.UserAgent != "patchbay-ami-check" {
		t.Errorf("contact User-Agent %q", c.UserAgent)
	}
	if left := time.Until(c.ExpiresAt); left < 590*time.Second || left > 610*time.Second {
		t.Errorf("contact expires in %s, want the 600 s the AOR grants", left)
	}

	// A resync through the actions: the NOTIFY arrives as sent.
	res = b.ami(user, pass, idx, op{Op: "resync", Arg: desk}, op{Op: "resync", Arg: laptop})
	if a := res[0].Action; a == nil || !a.Sent {
		t.Fatalf("resync %+v", res[0])
	}
	if a := res[1].Action; a == nil || a.Sent || a.Error != "no live contact" {
		t.Errorf("resync of an unregistered softphone: %+v", res[1])
	}
	n := nextLine(t, sip)
	if n.Notify == nil {
		t.Fatalf("no NOTIFY: %+v", n)
	}
	t.Logf("the NOTIFY as received: %q", n.Notify)
	for h, want := range map[string][]string{
		"Event": {"service-control"}, "Content-Type": {"text/plain"}, "Request-URI": {"sip:100@127.0.0.1:5170;transport=tcp"},
		// res_pjsip_notify adds its own Subscription-State first.
		"Subscription-State": {"terminated", "active"},
	} {
		if !slices.Equal(n.Notify[h], want) {
			t.Errorf("NOTIFY %s: %q, want %q", h, n.Notify[h], want)
		}
	}
	wantBody := []string{
		"action=restart",
		"RegisterCallId={" + reg.CallID + "}",
		"ConfigVersionStamp={00000000-0000-0000-0000-000000000000}",
		"DialplanVersionStamp={00000000-0000-0000-0000-000000000000}",
		"SoftkeyVersionStamp={00000000-0000-0000-0000-000000000000}",
		"FeatureControlVersionStamp={00000000-0000-0000-0000-000000000000}",
		"HeadsetVersionStamp={0-0000000000}",
	}
	if got := n.Notify["body"]; len(got) != 1 || got[0] != strings.Join(wantBody, "\r\n") {
		t.Errorf("NOTIFY body %q, want the lines %q joined by CRLF", got, wantBody)
	}

	// Swap to a version with a second phone: the reloads load it, and every
	// module apply reloads answers.
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	r2 := in.Resolved(t)
	b.writeVersion(r2)
	modules := []string{"res_pjsip.so", "pbx_config.so", "res_rtp_asterisk.so", "res_pjsip_notify.so", "indications", "logger", "manager"}
	ops := []op{}
	for _, m := range modules {
		ops = append(ops, op{Op: "reload", Arg: m})
	}
	ops = append(ops, op{Op: "endpoints"}, op{Op: "reload", Arg: "res_nope.so"})
	res = b.ami(user, pass, r2.Bundle.Index(), ops...)
	for i, m := range modules {
		if res[i].Error != "" {
			t.Errorf("reload %s: %s", m, res[i].Error)
		}
	}
	if got := res[len(modules)].Names; !slices.Equal(got, []string{desk, front, laptop}) {
		t.Errorf("endpoints after the swap and reload: %v", got)
	}
	if e := res[len(modules)+1].Error; e != "ami: Reload: No such module" {
		t.Errorf("reload of a missing module: %q", e)
	}

	// A version with no device: the endpoint list is empty, not an error.
	empty := house()
	empty.Phones, empty.Softphones, empty.People = nil, nil, nil
	r3 := empty.Resolved(t)
	b.writeVersion(r3)
	res = b.ami(user, pass, r3.Bundle.Index(), op{Op: "reload", Arg: "res_pjsip.so"}, op{Op: "reload", Arg: "pbx_config.so"},
		op{Op: "endpoints"}, op{Op: "contexts"})
	if res[0].Error != "" || res[1].Error != "" || len(res[2].Names) != 0 || res[2].Error != "" {
		t.Errorf("an empty bundle: %+v", res)
	}
	if !slices.Contains(res[3].Names, compile.DevicesContext) || !slices.Contains(res[3].Names, compile.UnavailableContext) {
		t.Errorf("contexts of an empty bundle: %v", res[3].Names)
	}

	// A new AMI password, as apply rolls it out: manager reloads over the
	// old session, then the client logs in with the new one.
	in.Secrets[intaketest.AMIRef] = []byte("not-a-secret-ami-0002")
	r4 := in.Resolved(t)
	b.writeVersion(r4)
	res = b.ami(user, pass, r4.Bundle.Index(), op{Op: "reload", Arg: "manager"}, op{Op: "endpoints"})
	if res[0].Error != "" {
		t.Fatalf("reload manager over the old session: %s", res[0].Error)
	}
	t.Logf("the old session after manager reloaded with a new password: endpoints %v, error %q", res[1].Names, res[1].Error)
	if res = b.ami(user, r4.AMIPassword, r4.Bundle.Index(), op{Op: "state"}); !res[0].State.Up {
		t.Errorf("login with the new password: %+v", res[0].State)
	}
	if res = b.ami(user, pass, r4.Bundle.Index(), op{Op: "state"}); res[0].State.Up {
		t.Error("the old password still logs in after manager reloaded")
	}
}
