package compile

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Streams in the bundle (release B, docs/plans/release-b.md §2(a), RT.1).
//
// A Stream that holds its number takes one of three forms in the dialplan:
//   - its source breaks streams.CheckSource (a Stream written before the
//     CRD's rules): its number plays "not available", a comment says why,
//     and it is not in streams.json, so a bad source never fails the
//     compile (the controller marks it InvalidSource);
//   - the install has no stream bridge (Settings.StreamBridge is empty):
//     its number plays "not available"; it is in streams.json all the same;
//   - otherwise its number answers, sets the call's time limit
//     (TIMEOUT(absolute), so Asterisk hangs up at it), and dials the
//     bridge through AudioSocket's channel driver at 16 kHz,
//     Dial(AudioSocket/<bridge>/<uuid>/c(slin16),,g); whatever ends the
//     dial (the bridge refused the call, failed, or cannot be reached)
//     continues to "not available".
//
// Only the stream's UUID (streams.UUID of its resource name) and number
// reach the dialplan, never its display name or its source, so a source
// change rewrites streams.json alone.

// streamTargets is the dialplan entry of each Stream holding its number,
// sorted by number, and the unsealed streams.json: every tuner pool, and
// each of those Streams with an acceptable source, keyed by UUID, with the
// pool its source's host is in.
func streamTargets(list []v1alpha1.Stream, claims Claims, s Settings) ([]dialTarget, streams.Document) {
	doc := streams.Document{Pools: []streams.Pool{}, Streams: map[string]streams.Stream{}}
	for _, p := range s.Streams.TunerPools {
		doc.Pools = append(doc.Pools, streams.Pool{Name: p.Name, Max: p.Max})
	}
	sorted := slices.Clone(list)
	slices.SortFunc(sorted, func(a, b v1alpha1.Stream) int { return cmp.Compare(a.Name, b.Name) })
	var targets []dialTarget
	for i := range sorted {
		st := &sorted[i]
		ref := ResourceRef{ResourceStream, st.Name}
		if !claims.Holds(ref) {
			continue
		}
		number, source := string(st.Spec.Number), st.Spec.Source
		if err := streams.CheckSource(source); err != nil {
			targets = append(targets, dialTarget{number: number, comment: ref.String() + ": its source is refused (" + commentText(err.Error()) + ")"})
			continue
		}
		id := streams.UUID(st.Name)
		doc.Streams[id] = streams.Stream{Name: st.Name, Number: number, Source: source, Pool: poolOf(s.Streams.TunerPools, source)}
		if s.StreamBridge == "" {
			targets = append(targets, dialTarget{number: number, comment: ref.String() + ": streams are disabled (no stream bridge)"})
			continue
		}
		targets = append(targets, dialTarget{number: number, comment: ref.String(), steps: []string{
			"Answer()",
			"Set(TIMEOUT(absolute)=" + strconv.FormatInt(int64(s.Streams.Limit()/time.Second), 10) + ")",
			"Dial(AudioSocket/" + s.StreamBridge + "/" + id + "/c(" + streams.Codec + "),,g)",
			unavailable,
		}})
	}
	return byNumber(targets), doc
}

// poolOf is the name of the tuner pool a source counts against: the pool
// one of whose Hosts is the source URL's host (url.URL.Hostname, so any
// port, and an IPv6 address without brackets), compared lower-case; or ""
// when it is in none (or is not a URL). A device named by another host (a
// DNS name for its IP) is not in the pool: list each form the Streams use.
func poolOf(pools []TunerPool, source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	for _, p := range pools {
		for _, h := range p.Hosts {
			if strings.ToLower(h) == host {
				return p.Name
			}
		}
	}
	return ""
}

// maxCommentText bounds the text a comment quotes from an error.
const maxCommentText = 200

// commentText is an error's text as a dialplan comment may quote it: no
// run of "@" (drivers.UserText, so it can never form a placeholder the
// agent would resolve), no control character, at most maxCommentText
// bytes. A source refused by CheckSource holds no white space, so the
// comment stays one line.
func commentText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, drivers.UserText(strings.ToValidUTF8(s, "?")))
	if len(s) <= maxCommentText {
		return s
	}
	cut := maxCommentText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
