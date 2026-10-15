package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// modelKeyWidth is the width of the key column of a model's profile.
const modelKeyWidth = 16

// supportLevels is the order features are listed in.
var supportLevels = []drivers.Support{drivers.Supported, drivers.Untested, drivers.Unsupported}

// Models prints what every driver in reg supports (design §4: the profiles
// are Go data, and this is how a person reads them): for each driver in
// registry order, each of its models (sorted) with its hardware, its
// provisioning (config file, default load, label limits, password length,
// resync), the PJSIP endpoint options the compiler renders for it and its
// features per signalling backend; then each firmware load of the
// driver's manifest with its source, models and files (name, size, and
// SHA-256 or "pending"), marking a load whose digests are not all known.
// It reads no cluster.
func Models(w io.Writer, reg *drivers.Registry) error {
	if reg == nil {
		return errors.New("models: no driver registry")
	}
	p := &printer{w: w}
	for i, d := range reg.Drivers() {
		if i > 0 {
			p.printf("\n")
		}
		p.printf("driver %s\n", d.Name())
		models := slices.Clone(d.Models())
		slices.Sort(models)
		for _, m := range models {
			prof, ok := d.Profile(m)
			if !ok {
				return fmt.Errorf("models: driver %s has no profile for its model %s", d.Name(), m)
			}
			p.printf("\n  model %s\n", m)
			printProfile(p, prof, d.Firmware())
		}
		loads := d.Firmware()
		if len(loads) == 0 {
			p.printf("\n  no firmware\n")
		}
		for _, l := range loads {
			p.printf("\n")
			printLoad(p, l)
		}
	}
	return p.err
}

// printProfile prints one model's profile as key/value lines (loads: its
// driver's manifest, to mark a default load missing from it); fields a
// model leaves zero (a softphone's screen) are left out.
func printProfile(p *printer, prof drivers.Profile, loads []drivers.FirmwareLoad) {
	kv := func(key string, lines ...string) {
		for i, line := range lines {
			if i > 0 {
				key = ""
			}
			p.printf("    %-*s%s\n", modelKeyWidth, key, line)
		}
	}
	hw := prof.Hardware
	if hw.LineKeys > 0 || hw.Softkeys > 0 {
		kv("hardware", fmt.Sprintf("%d line keys, %d softkeys", hw.LineKeys, hw.Softkeys))
	}
	if !zeroSize(hw.Display) {
		s := size(hw.Display)
		if !zeroSize(hw.ServiceArea) {
			s += ", service area " + size(hw.ServiceArea)
		}
		kv("display", s)
	}
	if !zeroSize(hw.Wallpaper) {
		s := size(hw.Wallpaper)
		if !zeroSize(hw.WallpaperThumbnail) {
			s += ", thumbnail " + size(hw.WallpaperThumbnail)
		}
		kv("wallpaper", s)
	}
	if hw.RingtoneFormat != "" {
		kv("ringtones", hw.RingtoneFormat)
	}
	if len(hw.Codecs) > 0 {
		kv("codecs", strings.Join(hw.Codecs, ", "))
	}

	pr := prof.Provisioning
	if pr.ConfigFile != "" {
		kv("config file", pr.ConfigFile)
	}
	if pr.DefaultFirmware != "" {
		def := pr.DefaultFirmware + " by default"
		if _, ok := drivers.LoadByName(loads, pr.DefaultFirmware); !ok {
			def += " (not in the manifest: a Phone of this model is Invalid until it is)"
		}
		kv("firmware", def)
	}
	var labels []string
	if pr.ScreenLabelMax > 0 {
		labels = append(labels, fmt.Sprintf("screen label at most %d characters", pr.ScreenLabelMax))
	}
	if pr.LineLabelMax > 0 {
		labels = append(labels, fmt.Sprintf("line label at most %d", pr.LineLabelMax))
	}
	if len(labels) > 0 {
		kv("labels", strings.Join(labels, ", "))
	}
	if pr.Password.Alphabet == drivers.Alphanumeric {
		kv("password", fmt.Sprintf("%d characters, letters and digits", pr.Password.Length))
	} else {
		kv("password", fmt.Sprintf("%d characters from %s", pr.Password.Length, pr.Password.Alphabet))
	}
	if pr.ResyncByNotify {
		kv("resync", "by NOTIFY (service-control)")
	} else {
		kv("resync", "not by NOTIFY")
	}

	e := prof.Endpoint
	qualify := "qualify off"
	if e.QualifyFrequency > 0 {
		qualify = "qualify every " + seconds(e.QualifyFrequency)
	}
	kv("endpoint",
		fmt.Sprintf("%s; allow %s; %s; registration expiry %s",
			e.Transport, strings.Join(e.Codecs, ", "), qualify, seconds(e.RegistrationExpiry)),
		fmt.Sprintf("force_rport %s, rewrite_contact %s, rtp_symmetric %s, direct_media %s",
			yesNo(e.ForceRport), yesNo(e.RewriteContact), yesNo(e.RTPSymmetric), yesNo(e.DirectMedia)),
		fmt.Sprintf("send_rpid %s, send_pai %s, trust_id_outbound %s",
			yesNo(e.SendRPID), yesNo(e.SendPAI), yesNo(e.TrustIDOutbound)),
	)

	backends := make([]drivers.Signalling, 0, len(prof.Features))
	for b := range prof.Features {
		backends = append(backends, b)
	}
	slices.Sort(backends)
	if len(backends) == 0 {
		kv("features", "none listed: every feature is unsupported")
	}
	for _, b := range backends {
		var lines []string
		for _, level := range supportLevels {
			var names []string
			for f, s := range prof.Features[b] {
				if s == level {
					names = append(names, string(f))
				}
			}
			slices.Sort(names)
			if level == drivers.Unsupported {
				names = append(names, "every feature not listed")
				if len(names) > 1 {
					names[len(names)-1] = "and " + names[len(names)-1]
				}
			}
			if len(names) > 0 {
				lines = append(lines, fmt.Sprintf("%s: %s", level, strings.Join(names, ", ")))
			}
		}
		kv(string(b)+" features", lines...)
	}
}

// printLoad prints one firmware load: source, models, and a table of its
// files.
func printLoad(p *printer, l drivers.FirmwareLoad) {
	header := "  firmware load " + l.Name
	if pending := len(l.PendingFiles()); pending > 0 {
		header += fmt.Sprintf(" (SHA-256 pending for %d of %d files: not loadable yet)", pending, len(l.Files))
	}
	p.printf("%s\n", header)
	p.printf("    %-*s%s\n", modelKeyWidth, "source", l.Source)
	p.printf("    %-*s%s\n", modelKeyWidth, "models", strings.Join(l.Models, ", "))
	p.printf("    files\n")
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, f := range l.Files {
		digest := f.SHA256
		if f.Shared {
			digest += "  shared"
		}
		_, _ = fmt.Fprintf(tw, "      %s\t%d\t%s\n", f.Name, f.Size, digest)
	}
	_ = tw.Flush() // into a strings.Builder: cannot fail
	p.printf("%s", b.String())
}

func zeroSize(s drivers.Size) bool { return s.Width == 0 && s.Height == 0 }

func size(s drivers.Size) string { return fmt.Sprintf("%dx%d", s.Width, s.Height) }

func seconds(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) + "s" }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
