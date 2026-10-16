//go:build image

// Package image checks the stream bridge's image as built
// (docs/plans/release-b.md §6.3 PK.1): `make image-check-stream-bridge`
// runs it against IMG_STREAM_BRIDGE (the env PATCHBAY_STREAM_BRIDGE_IMAGE)
// after `make image-stream-bridge`, and CI against the amd64 build of its
// image job. Every container runs as the chart runs the bridge (no
// network, a read-only root, no capabilities, no new privileges), so
// nothing here reaches anything but the container's own loopback.
package image

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// defaultImage is the Makefile's IMG_STREAM_BRIDGE default.
const defaultImage = "ghcr.io/christopherjmiller/patchbay-stream-bridge:dev"

// The image's contract with the chart and the bridge.
const (
	wantUser     = "65532:65532"
	wantUID      = "65532"
	wantLicenses = "Apache-2.0 AND GPL-3.0-or-later"
	caBundle     = "/etc/ssl/certs/ca-certificates.crt"
	patchbayPath = "/patchbay"
)

func image() string {
	if v := os.Getenv("PATCHBAY_STREAM_BRIDGE_IMAGE"); v != "" {
		return v
	}
	return defaultImage
}

// docker runs the docker CLI and returns its stdout and stderr.
func docker(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	bin, lerr := exec.LookPath("docker")
	if lerr != nil {
		t.Fatal("docker is not on PATH: the image check needs it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// run runs entrypoint with args in the image, confined as the chart runs
// the bridge.
func run(t *testing.T, entrypoint string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runWith(t, nil, entrypoint, args...)
}

// runWith is run with more docker run options (a mount, an environment).
func runWith(t *testing.T, opts []string, entrypoint string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	all := []string{
		"run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
	}
	all = append(all, opts...)
	all = append(all, "--entrypoint", entrypoint, image())
	return docker(t, append(all, args...)...)
}

// mustRun is run, failing the test unless it exits 0.
func mustRun(t *testing.T, entrypoint string, args ...string) string {
	t.Helper()
	stdout, stderr, err := run(t, entrypoint, args...)
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s%s", entrypoint, args, image(), err, stdout, stderr)
	}
	return stdout
}

func TestMain(m *testing.M) {
	// One clear failure, not one per test, when the image was never built.
	cmd := exec.CommandContext(context.Background(), "docker", "image", "inspect", "--format", "{{.Id}}", image())
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "image %s is not in the local docker (%v): run `nix develop -c make image-stream-bridge` first\n%s", image(), err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestImageConfig: the image runs patchbay as 65532:65532 and says what it
// is and under which licences (Apache-2.0 patchbay, GPL-3.0-or-later
// ffmpeg).
func TestImageConfig(t *testing.T) {
	stdout, stderr, err := docker(t, "image", "inspect", "--format", "{{json .Config}}", image())
	if err != nil {
		t.Fatalf("docker image inspect: %v\n%s", err, stderr)
	}
	var cfg struct {
		User       string            `json:"User"`
		Entrypoint []string          `json:"Entrypoint"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("image config: %v\n%s", err, stdout)
	}
	if cfg.User != wantUser {
		t.Errorf("the image's user is %q, want %q", cfg.User, wantUser)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != patchbayPath {
		t.Errorf("the entrypoint is %q, want [%s]", cfg.Entrypoint, patchbayPath)
	}
	for k, want := range map[string]string{
		"org.opencontainers.image.licenses": wantLicenses,
		"org.opencontainers.image.source":   "https://github.com/ChristopherJMiller/patchbay",
	} {
		if got := cfg.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if d := cfg.Labels["org.opencontainers.image.description"]; !strings.Contains(d, "stream bridge") || !strings.Contains(d, "ffmpeg") {
		t.Errorf("the description %q does not say it is the stream bridge with ffmpeg", d)
	}
}

// TestImageUser: PID 1 in the image runs as 65532:65532 with no
// capabilities (the image's USER, as the chart's securityContext repeats).
func TestImageUser(t *testing.T) {
	status := mustRun(t, "/bin/cat", "/proc/1/status")
	fields := map[string]string{}
	for line := range strings.SplitSeq(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	for k, want := range map[string]string{
		"Uid":    strings.Repeat(wantUID+" ", 3) + wantUID,
		"Gid":    strings.Repeat(wantUID+" ", 3) + wantUID,
		"CapEff": "0000000000000000",
	} {
		if fields[k] != want {
			t.Errorf("PID 1's %s is %q, want %q", k, fields[k], want)
		}
	}
}

// TestImagePatchbayRuns: the static binary runs on Alpine's musl and says
// its version.
func TestImagePatchbayRuns(t *testing.T) {
	if out := mustRun(t, patchbayPath, "version"); !strings.HasPrefix(out, "patchbay ") {
		t.Errorf("patchbay version said %q", out)
	}
}

// TestImageFFmpeg: ffmpeg is where the bridge looks for it
// (streambridge.DefaultFFmpeg) and runs.
func TestImageFFmpeg(t *testing.T) {
	out := mustRun(t, streambridge.DefaultFFmpeg, "-version")
	first, _, _ := strings.Cut(out, "\n")
	if !regexp.MustCompile(`^ffmpeg version \d+\.\d+`).MatchString(first) {
		t.Errorf("ffmpeg -version's first line is %q", first)
	}
	t.Log(first)
}

// TestImageFFmpegLicence: the image's ffmpeg is redistributable under the
// licence the image's label names: built with --enable-gpl, never with
// --enable-nonfree, and GPL version 3 or later by its own account (-L).
func TestImageFFmpegLicence(t *testing.T) {
	conf := strings.Fields(mustRun(t, streambridge.DefaultFFmpeg, "-hide_banner", "-buildconf"))
	has := func(opt string) bool {
		for _, f := range conf {
			if f == opt {
				return true
			}
		}
		return false
	}
	if !has("--enable-gpl") {
		t.Error("ffmpeg's build configuration lacks --enable-gpl")
	}
	if has("--enable-nonfree") {
		t.Error("ffmpeg is built with --enable-nonfree: the image is not redistributable")
	}
	// -L prints the licence ffmpeg was built under; the image's label says
	// GPL-3.0-or-later.
	lic := strings.Join(strings.Fields(mustRun(t, streambridge.DefaultFFmpeg, "-hide_banner", "-L")), " ")
	if !strings.Contains(lic, "GNU General Public License") || !strings.Contains(lic, "version 3 of the License, or (at your option) any later version") {
		t.Errorf("ffmpeg -L does not say GPL version 3 or later (the image's label says %s):\n%s", wantLicenses, lic)
	}
}

// TestImageCABundle: https sources are checked against the system's trust
// store (FFmpegArgs with no caFile), which Alpine's ca-certificates
// provides.
func TestImageCABundle(t *testing.T) {
	out := mustRun(t, "/bin/cat", caBundle)
	if n := strings.Count(out, "-----BEGIN CERTIFICATE-----"); n < 10 {
		t.Errorf("%s holds %d certificates", caBundle, n)
	}
}

// TestImageFFmpegTakesTheContractArgs: the image's ffmpeg (Alpine's, not
// the dev shell's) accepts every option of streambridge.FFmpegArgs, the
// https-only ones included: with nothing listening on the container's
// loopback, each source fails with "Connection refused", never on an
// option.
func TestImageFFmpegTakesTheContractArgs(t *testing.T) {
	for _, tc := range []struct{ source, caFile string }{
		{"http://127.0.0.1:1/x", ""},
		{"https://127.0.0.1:1/x", ""},
		{"https://127.0.0.1:1/x", caBundle},
	} {
		t.Run(tc.source+" "+tc.caFile, func(t *testing.T) {
			args := streambridge.FFmpegArgs(tc.source, tc.caFile)
			stdout, stderr, err := run(t, streambridge.DefaultFFmpeg, args...)
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("ffmpeg %v: %v (want it to run and fail)\n%s%s", args, err, stdout, stderr)
			}
			for _, bad := range []string{"not found", "Unrecognized option", "Invalid option", "Error splitting the argument list"} {
				if strings.Contains(stderr, bad) {
					t.Errorf("ffmpeg refused an option (%q):\n%s", bad, stderr)
				}
			}
			if !strings.Contains(stderr, "Connection refused") {
				t.Errorf("ffmpeg did not fail on the connection:\n%s", stderr)
			}
			if stdout != "" {
				t.Errorf("ffmpeg wrote %d bytes of output", len(stdout))
			}
		})
	}
}

// decodeScript runs inside the image: ffmpeg serves a generated 2 s tone
// (48 kHz stereo WAV, as the e2e fake sources do) on the container's
// loopback at $URL (https with the certificate in /certs), and the bridge's
// decoder, ffmpeg with the arguments passed ("$@", FFmpegArgs), reads it,
// retried while the server is not listening yet. It prints the decoded
// byte count, then the decoder's stderr.
const decodeScript = `
tls=""
case "$URL" in https:*) tls="-cert_file /certs/cert.pem -key_file /certs/key.pem" ;; esac
# shellcheck disable=SC2086
ffmpeg -nostdin -hide_banner -loglevel error -f lavfi -i sine=frequency=1000:sample_rate=48000:duration=2 \
  -ac 2 -f wav -listen 1 $tls "$URL" 2>/tmp/server.err &
i=0
while [ $i -lt 40 ]; do
  i=$((i + 1))
  if ffmpeg "$@" >/tmp/out.pcm 2>/tmp/err; then break; fi
  grep -q "Connection refused" /tmp/err || break
  sleep 0.25
done
kill %1 2>/dev/null
echo "decoded $(wc -c </tmp/out.pcm)"
cat /tmp/err
`

// writeCert writes a throwaway self-signed certificate for 127.0.0.1 and
// its key, made now (hard rule 4), into a directory the image's user can
// read, and returns the directory.
func writeCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "patchbay image check"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The container's user is not this one: the directory and files must
	// be world-readable.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"cert.pem": {Type: "CERTIFICATE", Bytes: der},
		"key.pem":  {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestImageFFmpegDecodes: in the image, the bridge's decoder (Alpine's
// ffmpeg with streambridge.FFmpegArgs) decodes a WAV served over http, and
// over https with the server's certificate as caFile: 2 s of 16 kHz mono
// 16-bit PCM. Without caFile the system's trust store refuses the
// self-signed certificate: -tls_verify is in force. Everything stays on
// the container's loopback.
func TestImageFFmpegDecodes(t *testing.T) {
	certs := writeCert(t)
	const twoSeconds = 2 * streams.SampleRate * 2 // 16-bit mono
	for _, tc := range []struct {
		name, url, caFile string
		ok                bool
	}{
		{"http", "http://127.0.0.1:8080/tone", "", true},
		{"https", "https://127.0.0.1:8443/tone", "/certs/cert.pem", true},
		{"https untrusted", "https://127.0.0.1:8443/tone", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []string{"-v", certs + ":/certs:ro", "-e", "URL=" + tc.url}
			args := append([]string{"-c", decodeScript, "sh"}, streambridge.FFmpegArgs(tc.url, tc.caFile)...)
			stdout, stderr, err := runWith(t, opts, "/bin/sh", args...)
			if err != nil {
				t.Fatalf("the decode script: %v\n%s%s", err, stdout, stderr)
			}
			first, rest, _ := strings.Cut(stdout, "\n")
			n, err := strconv.Atoi(strings.TrimPrefix(first, "decoded "))
			if err != nil {
				t.Fatalf("the decode script said %q", stdout)
			}
			if !tc.ok {
				if n != 0 || !strings.Contains(strings.ToLower(rest), "certificate") {
					t.Errorf("an untrusted certificate: %d bytes decoded, and the error does not mention the certificate:\n%s", n, rest)
				}
				return
			}
			if n < twoSeconds*9/10 || n > twoSeconds*11/10 {
				t.Errorf("decoded %d bytes, want 2 s of PCM (%d ± 10%%):\n%s", n, twoSeconds, rest)
			}
			if strings.TrimSpace(rest) != "" {
				t.Errorf("the decoder complained:\n%s", rest)
			}
		})
	}
}
