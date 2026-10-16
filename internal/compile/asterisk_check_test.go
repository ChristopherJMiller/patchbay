//go:build asterisk

package compile_test

// The real-Asterisk check (make asterisk-check): every golden bundle,
// compiled with the built-in drivers, boots in the Asterisk image the chart
// pins, the way the PBX pod runs it, with a clean boot log, and Asterisk
// reports exactly the modules, endpoints, auths, AORs, transports,
// dialplan and AMI account the bundle defines. Then, as each device, a SIP
// client registers (refused with a wrong password or a contact outside the
// allowed networks; granted the AOR's expiry with the right one) and calls
// the echo test and a reserved number. TestRealAsteriskACL proves the ACL
// refuses everything outside the allowed networks.
//
// Docker may be rootless (container addresses unreachable from the host),
// so the container gets no network at all (--network none), the bundle's
// PBX address is 127.0.0.1 with 127.0.0.0/8 allowed, and everything runs
// through docker exec: the Asterisk CLI, and this test binary itself as
// the SIP client (sipclient_asterisk_test.go), which is why it must be
// static (CGO_ENABLED=0).

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

const (
	// checkLabel marks every container this check starts, with the PID of
	// the test process that owns it as the value, so a run cut short can be
	// cleaned up: make asterisk-check removes only containers whose owner
	// is gone, never those of a run still going in another shell.
	checkLabel = "patchbay.chrismiller.xyz/asterisk-check"
	// asteriskConf is where the PBX pod's Asterisk reads its config.
	asteriskConf = compile.AsteriskConfigDir + "/asterisk.conf"
	// bootTimeout bounds how long Asterisk may take to be fully booted.
	bootTimeout = 90 * time.Second
)

// chartAsteriskImage is the Asterisk image the chart pins, repository:tag@digest.
func chartAsteriskImage(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "patchbay", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		PBX struct {
			Asterisk struct {
				Image struct {
					Repository, Tag, Digest string
				} `json:"image"`
			} `json:"asterisk"`
		} `json:"pbx"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	img := values.PBX.Asterisk.Image
	if img.Repository == "" || img.Tag == "" || !strings.HasPrefix(img.Digest, "sha256:") {
		t.Fatalf("charts/patchbay/values.yaml pins no Asterisk image by digest: %+v", img)
	}
	return img.Repository + ":" + img.Tag + "@" + img.Digest
}

// docker runs the docker CLI and returns its combined output.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// asteriskContainer is one running Asterisk.
type asteriskContainer struct {
	t    *testing.T
	name string
}

// cli runs one Asterisk CLI command through the control socket.
func (c asteriskContainer) cli(command string) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := docker(ctx, "exec", c.name, "/usr/sbin/asterisk", "-C", asteriskConf, "-rx", command)
	if err != nil {
		c.t.Fatalf("asterisk -rx %q: %v", command, err)
	}
	return noEID.ReplaceAllString(out, "")
}

// noEID is what every asterisk process prints on a host without an Ethernet
// interface, as a --network none container is: the remote console says it
// before each answer. The PBX pod, on the host network, has one.
var noEID = regexp.MustCompile(`(?m)^No ethernet interface found for seeding global EID\. You will have to set it manually\.\n`)

// startAsterisk resolves the bundle's placeholders with the test's fake
// secrets, lays its asterisk/ tree out as the agent does (v<version>/ and
// the current symlink), and runs the pinned image the way the chart does:
// uid and gid 1000, read-only root, no capabilities, no new privileges,
// writable emptyDir-like tmpfs mounts, the config mounted read-only.
func startAsterisk(t *testing.T, env checkEnv, name string, b compile.Bundle, secrets map[compile.SecretRef][]byte) asteriskContainer {
	t.Helper()
	live := filepath.Join(t.TempDir(), "live")
	version := "v" + b.Version[:16]
	for p, content := range b.Files {
		if !strings.HasPrefix(p, compile.TreeAsterisk+"/") {
			continue
		}
		var unresolved []string
		resolved := compile.PlaceholderPattern.ReplaceAllFunc(content, func(m []byte) []byte {
			sub := compile.PlaceholderPattern.FindSubmatch(m)
			v, ok := secrets[compile.SecretRef{Name: string(sub[1]), Key: string(sub[2])}]
			if !ok {
				unresolved = append(unresolved, string(m))
			}
			return v
		})
		if len(unresolved) > 0 {
			t.Fatalf("%s: no test value for %v", p, unresolved)
		}
		dst := filepath.Join(live, version, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		// World-readable: with rootless Docker the container's uid 1000 is
		// not the file's owner. The values are the test's fake secrets.
		if err := os.WriteFile(dst, resolved, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(version, filepath.Join(live, compile.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Dir(live), func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o755)
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmpfs := "uid=1000,gid=1000,mode=0750"
	args := []string{
		"run", "--detach", "--name", name, "--label", checkLabel + "=" + strconv.Itoa(os.Getpid()),
		"--network", "none",
		"--user", "1000:1000", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/var/run/asterisk:" + tmpfs,
		"--tmpfs", "/var/spool/asterisk:" + tmpfs,
		"--tmpfs", "/var/log/asterisk:" + tmpfs,
		"--tmpfs", recordDir + ":" + tmpfs,
		"--mount", "type=bind,source=" + live + ",target=" + compile.LiveDir + ",readonly",
		"--mount", "type=bind,source=" + env.clientDir + ",target=" + clientDir + ",readonly",
		"--entrypoint", "/usr/sbin/asterisk",
		env.image, "-f", "-n", "-vvv", "-C", asteriskConf,
	}
	if _, err := docker(ctx, args...); err != nil {
		t.Fatal(err)
	}
	c := asteriskContainer{t: t, name: name}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if t.Failed() {
			logs, _ := docker(ctx, "logs", name)
			t.Logf("asterisk log of %s:\n%s", name, logs)
		}
		if out, err := docker(ctx, "rm", "--force", name); err != nil {
			t.Errorf("removing %s: %v\n%s", name, err, out)
		}
	})

	deadline := time.Now().Add(bootTimeout)
	for {
		out, err := docker(ctx, "exec", name, "/usr/sbin/asterisk", "-C", asteriskConf, "-rx", "core waitfullybooted")
		if err == nil && strings.Contains(out, "fully booted") {
			return c
		}
		if running, _ := docker(ctx, "inspect", "--format", "{{.State.Running}}", name); strings.TrimSpace(running) != "true" {
			t.Fatalf("Asterisk exited before it was fully booted (%v)", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Asterisk is not fully booted after %s: %v", bootTimeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkEnv is what every case of the check shares.
type checkEnv struct {
	image     string // the chart's Asterisk image, by digest
	clientDir string // a host directory holding this test binary, the SIP client
}

// clientDir is where the container sees checkEnv.clientDir.
const clientDir = "/opt/patchbay-check"

// newCheckEnv finds the image and copies this (static) test binary where
// the containers can run it as the SIP client.
func newCheckEnv(t *testing.T) checkEnv {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	requireStatic(t, self)
	dir := t.TempDir()
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return checkEnv{image: chartAsteriskImage(t), clientDir: dir}
}

// checkSettings adapts a case's settings to the container: Asterisk
// binds 127.0.0.1 (the only address it has), and the ACL admits it unless
// admitLoopback is false.
func checkSettings(s compile.Settings, admitLoopback bool) compile.Settings {
	s.PBXAddress = "127.0.0.1"
	if admitLoopback {
		s.AllowedNetworks = append(s.AllowedNetworks, "127.0.0.0/8")
	}
	return s
}

// TestRealAsterisk boots each golden bundle, checks what Asterisk loaded, and
// registers and calls as each device.
func TestRealAsterisk(t *testing.T) {
	env := newCheckEnv(t)
	reg := builtinRegistry(t)
	for _, name := range goldenCases(t) {
		t.Run(name, func(t *testing.T) {
			in, s := loadGolden(t, name)
			s = checkSettings(s, true)
			b, err := compile.Compile(reg, in, s)
			if err != nil {
				t.Fatal(err)
			}
			c := startAsterisk(t, env, fmt.Sprintf("patchbay-asterisk-check-%d-%s", os.Getpid(), name), b, in.Secrets)
			checkAsterisk(t, c, b, s)
			checkDialplan(t, c, b, s.NumberPlan)
			checkSIP(t, c, b, s, in.Secrets)
		})
	}
}

// checkAsterisk asserts that the running Asterisk holds what the bundle
// defines.
func checkAsterisk(t *testing.T, c asteriskContainer, b compile.Bundle, s compile.Settings) {
	t.Helper()
	if v := c.cli("core show version"); !strings.HasPrefix(v, "Asterisk 22.") {
		t.Errorf("core show version: %q", v)
	}

	// Every module modules.conf lists is running.
	modulesConf, err := parseConf(b.Files["asterisk/modules.conf"])
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, sec := range modulesConf {
		modules = append(modules, sec.get("load")...)
	}
	if len(modules) == 0 {
		t.Fatal("modules.conf loads nothing")
	}
	shown := c.cli("module show")
	for _, m := range modules {
		if !moduleRunning(shown, m) {
			t.Errorf("module %s is not Running", m)
		}
	}

	// Endpoints, auths and AORs are exactly the routed devices'.
	var ids, numbers []string
	for id, d := range b.Devices {
		if d.Version != "" {
			ids = append(ids, id)
			numbers = append(numbers, d.Number)
		}
	}
	slices.Sort(ids)
	slices.Sort(numbers)
	if got := cliObjects(c.cli("pjsip show endpoints"), "Endpoint:"); !slices.Equal(got, ids) {
		t.Errorf("pjsip show endpoints = %v, want %v", got, ids)
	}
	if got := cliObjects(c.cli("pjsip show auths"), "Auth:"); !slices.Equal(got, ids) {
		t.Errorf("pjsip show auths = %v, want %v", got, ids)
	}
	if got := cliObjects(c.cli("pjsip show aors"), "Aor:"); !slices.Equal(got, numbers) {
		t.Errorf("pjsip show aors = %v, want %v", got, numbers)
	}
	transports := c.cli("pjsip show transports")
	for _, tr := range []string{compile.TransportUDP, compile.TransportTCP} {
		if !regexp.MustCompile(regexp.QuoteMeta(tr) + `\s+\S+\s+\d+\s+\d+\s+` + regexp.QuoteMeta(fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort))).MatchString(transports) {
			t.Errorf("pjsip show transports has no %s on %s:%d:\n%s", tr, s.PBXAddress, s.SIPPort, transports)
		}
	}

	// Every context and extension of extensions.conf is in the dialplan.
	extensions, err := parseConf(b.Files["asterisk/extensions.conf"])
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range extensions {
		if sec.Name == "general" {
			continue
		}
		plan := c.cli("dialplan show " + sec.Name)
		if !strings.Contains(plan, "[ Context '"+sec.Name+"' created by 'pbx_config' ]") {
			t.Errorf("context %s is not in the dialplan:\n%s", sec.Name, plan)
			continue
		}
		for _, e := range sec.get("exten") {
			ext := strings.SplitN(e, ",", 2)[0]
			if !strings.Contains(plan, "'"+ext+"' =>") {
				t.Errorf("extension %s@%s is not in the dialplan:\n%s", ext, sec.Name, plan)
			}
		}
	}

	// AMI: on loopback, and the agent's account has only its classes.
	if settings := c.cli("manager show settings"); !regexp.MustCompile(`127\.0\.0\.1:` + fmt.Sprint(compile.AMIPort)).MatchString(settings) {
		t.Errorf("AMI is not bound to 127.0.0.1:%d:\n%s", compile.AMIPort, settings)
	}
	if users := c.cli("manager show users"); cliCount(users, compile.AMIUsername) != 1 || !strings.Contains(users, "1 manager users configured.") {
		t.Errorf("manager show users, want only %s:\n%s", compile.AMIUsername, users)
	}
	user := c.cli("manager show user " + compile.AMIUsername)
	for _, field := range []string{"read perm", "write perm"} {
		m := regexp.MustCompile(`(?m)^\s*` + field + `:\s*(\S*)\s*$`).FindStringSubmatch(user)
		if m == nil || m[1] != compile.AMIClasses {
			t.Errorf("manager user %s %s, want %s:\n%s", compile.AMIUsername, field, compile.AMIClasses, user)
		}
	}
	if http := c.cli("http show status"); !strings.Contains(http, "Server Disabled") {
		t.Errorf("Asterisk's HTTP server is not disabled:\n%s", http)
	}

	// Asterisk keeps going past most config mistakes (an unknown option,
	// a file it cannot read), logging them: the boot log must be clean.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logs, err := docker(ctx, "logs", c.name)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(logs, "\n") {
		if bootLogProblem.MatchString(line) && !slices.Contains(bootLogKnown, strings.TrimSpace(line)) {
			t.Errorf("Asterisk logged: %s", line)
		}
	}
}

// bootLogProblem matches a log line at WARNING or ERROR, or a config
// problem Asterisk prints before its logger starts.
var bootLogProblem = regexp.MustCompile(`(WARNING|ERROR)\[|Unable to load config|Could not find option|Could not load`)

// bootLogKnown are lines Asterisk 22.11 prints whatever its config: the
// image's own sample config prints them too (checked 2026-10-15).
var bootLogKnown = []string{
	"Could not find option 'minimum_size' with type 'threadpool' in module 'stasis'",
}

// cliObjects lists the object names a `pjsip show <objects>` table prints
// after label, sorted, without the header row ("<Endpoint/CID...>") and
// without the "/<caller ID>" Asterisk appends to an endpoint's name.
func cliObjects(out, label string) []string {
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != label || strings.HasPrefix(f[1], "<") {
			continue
		}
		names = append(names, strings.SplitN(f[1], "/", 2)[0])
	}
	slices.Sort(names)
	return names
}

// cliCount counts the lines holding word as a field.
func cliCount(out, word string) int {
	n := 0
	for line := range strings.SplitSeq(out, "\n") {
		if slices.Contains(strings.Fields(line), word) {
			n++
		}
	}
	return n
}

// moduleRunning reports whether `module show` lists name as Running.
func moduleRunning(moduleShow, name string) bool {
	for line := range strings.SplitSeq(moduleShow, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == name && strings.Contains(line, "Running") {
			return true
		}
	}
	return false
}

// sip runs the SIP client inside the container with env, and returns its
// result line.
func (c asteriskContainer) sip(mode string, env map[string]string) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"exec", "-e", sipClientEnv + "=" + mode}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	out, err := docker(ctx, append(args, c.name, clientDir+"/client")...)
	if err != nil {
		c.t.Fatalf("SIP client %s %v: %v", mode, env, err)
	}
	return strings.TrimSpace(noEID.ReplaceAllString(out, ""))
}

// checkDialplan asserts which extension Asterisk picks for sample numbers:
// an exact extension over any pattern, a reserved pattern over the
// catch-alls, and the catch-alls for anything else.
func checkDialplan(t *testing.T, c asteriskContainer, b compile.Bundle, plan compile.NumberPlan) {
	t.Helper()
	extensions, err := parseConf(b.Files["asterisk/extensions.conf"])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{} // number -> the extension that must answer it
	for _, sec := range extensions {
		if sec.Name != compile.DevicesContext {
			continue
		}
		for _, e := range sec.get("exten") {
			if ext := strings.SplitN(e, ",", 2)[0]; !strings.HasPrefix(ext, "_") {
				want[ext] = ext
			}
		}
	}
	expect := func(number string) {
		if _, exact := want[number]; exact {
			return
		}
		for _, r := range plan.Reserved {
			if compile.MatchPattern(r, number) {
				if strings.ContainsAny(r, "X.") {
					r = "_" + r
				}
				want[number] = r
				return
			}
		}
		if number[0] >= '0' && number[0] <= '9' {
			want[number] = "_X!"
		} else {
			want[number] = "_[*#]!"
		}
	}
	for _, r := range plan.Reserved {
		sample, variable := strings.CutSuffix(r, ".")
		sample = strings.ReplaceAll(sample, "X", "5")
		if variable {
			sample += "55"
		}
		expect(sample)
	}
	for _, n := range []string{"1999999", "*9", "#1"} {
		expect(n)
	}
	first := regexp.MustCompile(`'([^']+)' =>`)
	for _, n := range slices.Sorted(maps.Keys(want)) {
		out := c.cli("dialplan show " + n + "@" + compile.DevicesContext)
		if m := first.FindStringSubmatch(out); m == nil || m[1] != want[n] {
			t.Errorf("dialling %s reaches %v, want '%s':\n%s", n, m, want[n], out)
		}
	}
}

// checkSIP registers as each rendered device, with a wrong password and
// then the right one, and calls the echo test and a reserved number.
func checkSIP(t *testing.T, c asteriskContainer, b compile.Bundle, s compile.Settings, secrets map[compile.SecretRef][]byte) {
	t.Helper()
	objects, err := pjsipObjects(b.Files["asterisk/pjsip.conf"])
	if err != nil {
		t.Fatal(err)
	}
	server := fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort)
	var ids []string
	for id, d := range b.Devices {
		if d.Version != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		// Nobody can register: there is no endpoint to be.
		out := c.sip("register", map[string]string{
			"SIP_SERVER": server, "SIP_TRANSPORT": "udp", "SIP_USER": "100", "SIP_AUTH_USER": "phone-nobody",
			"SIP_PASSWORD": "not-a-secret", "SIP_PORT": fmt.Sprint(s.SIPPort + 100), "SIP_EXPIRES": "600",
		})
		if strings.HasPrefix(out, "status=200") {
			t.Errorf("a REGISTER with no endpoint behind it was accepted: %s", out)
		}
		return
	}
	reserved := strings.ReplaceAll(strings.TrimSuffix(s.NumberPlan.Reserved[0], "."), "X", "5")
	for i, id := range ids {
		d := b.Devices[id]
		ep, aor := objects[[2]string{"endpoint", id}], objects[[2]string{"aor", d.Number}]
		transport, _ := ep.one("transport")
		maxExp, _ := aor.one("maximum_expiration")
		port := s.SIPPort + 100 + 10*i
		env := func(port int, extra ...string) map[string]string {
			e := map[string]string{
				"SIP_SERVER": server, "SIP_TRANSPORT": strings.TrimPrefix(transport, "transport-"),
				"SIP_USER": d.Number, "SIP_AUTH_USER": id, "SIP_PASSWORD": string(secrets[compile.PasswordRef(id)]),
				"SIP_PORT": fmt.Sprint(port), "SIP_EXPIRES": "3600",
			}
			for j := 0; j+1 < len(extra); j += 2 {
				e[extra[j]] = extra[j+1]
			}
			return e
		}

		if out := c.sip("register", env(port+1, "SIP_PASSWORD", "wrong-password")); !regexp.MustCompile(`^status=40[13] challenged=true `).MatchString(out) {
			t.Errorf("%s: REGISTER with a wrong password: %s, want a challenge, then 401 or 403", id, out)
		}
		// Challenged, then registered: asked for 3600 s, granted the AOR's
		// maximum (the expiry the device's own config asks for; the 200
		// says what is left of it, so a second less is fine). Asterisk
		// keeps the Contact as sent, unless the endpoint rewrites it to
		// where the REGISTER came from.
		out := c.sip("register", env(port))
		m := registerResult.FindStringSubmatch(out)
		contact := fmt.Sprintf("sip:%s@127.0.0.1:%d;transport=%s", d.Number, port, strings.TrimPrefix(transport, "transport-"))
		if rewrite, _ := ep.one("rewrite_contact"); rewrite == "yes" {
			contact = fmt.Sprintf(`sip:%s@127.0.0.1:\d+`, d.Number)
		} else {
			contact = regexp.QuoteMeta(contact)
		}
		if m == nil {
			m = make([]string, 5)
		}
		granted, _ := strconv.Atoi(m[3])
		limit, _ := strconv.Atoi(maxExp)
		if m[1] != "200" || m[2] != "true" || granted > limit || granted < limit-2 ||
			!regexp.MustCompile("^"+contact+"$").MatchString(m[4]) {
			t.Errorf("%s: REGISTER: %s, want a challenge, then 200 for %s s with contact %s", id, out, maxExp, contact)
		}
		if out := c.sip("call", env(port+2, "SIP_TARGET", s.NumberPlan.EchoTest)); out != "status=200" {
			t.Errorf("%s: calling the echo test %s: %s, want status=200", id, s.NumberPlan.EchoTest, out)
		}
		if i == 0 {
			if out := c.sip("call", env(port+3, "SIP_TARGET", reserved)); out != "status=200" {
				t.Errorf("%s: calling reserved %s: %s, want status=200 (answered by the unavailable prompt)", id, reserved, out)
			}
			// The endpoint's contact ACL refuses it (Asterisk then
			// challenges again, so the client ends on 401).
			if out := c.sip("register", env(port+4, "SIP_CONTACT_HOST", "198.51.100.7")); !regexp.MustCompile(`^status=40[13] `).MatchString(out) {
				t.Errorf("%s: REGISTER of a contact outside the allowed networks: %s, want 401 or 403", id, out)
			}
			if contacts := c.cli("pjsip show contacts"); strings.Contains(contacts, "198.51.100.7") {
				t.Errorf("a contact outside the allowed networks was bound:\n%s", contacts)
			}
		}
	}
}

// registerResult parses the client's REGISTER line: status, challenged,
// granted expiry and bound contact.
var registerResult = regexp.MustCompile(`^status=(\d+) challenged=(true|false) expires=(\d*) contact=(\S*)$`)

// TestRealAsteriskACL boots the house bundle without the container's own
// address among the allowed networks: Asterisk must refuse its requests.
func TestRealAsteriskACL(t *testing.T) {
	env := newCheckEnv(t)
	in, s := loadGolden(t, "house")
	s = checkSettings(s, false)
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	c := startAsterisk(t, env, fmt.Sprintf("patchbay-asterisk-check-%d-acl", os.Getpid()), b, in.Secrets)
	id := drivers.DeviceID(drivers.KindPhone, "desk")
	out := c.sip("register", map[string]string{
		"SIP_SERVER": fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort), "SIP_TRANSPORT": "tcp", "SIP_USER": "100",
		"SIP_AUTH_USER": id, "SIP_PASSWORD": string(in.Secrets[compile.PasswordRef(id)]),
		"SIP_PORT": fmt.Sprint(s.SIPPort + 100), "SIP_EXPIRES": "600",
	})
	if !strings.HasPrefix(out, "status=403 challenged=false") {
		t.Errorf("REGISTER from outside the allowed networks: %s, want status=403 without a challenge", out)
	}
}
