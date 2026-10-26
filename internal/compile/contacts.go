package compile

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The softphones' contacts: ContactsFile, the phones' two directories
// (directory.go) as one vCard 4.0 list, which Linphone downloads from the
// URL its provisioning document names (misc/contacts-vcard-list, written by
// internal/drivers/softphone) at every start and shows as read-only
// contacts. Each card is a name and the SIP URI that dials its number on
// the PBX; Linphone sorts them itself, and has no 32-entry limit.
//
// Linphone parses the file with belcard. Checked in linphone-sdk
// (https://github.com/BelledonneCommunications/linphone-sdk) at commit
// 79aefb33f61261095a1e8b1e4c9a5aafbcf8c642 (2026-10-26):
//
//   - belcard/src/vcard_rules lines 3-6: a card is BEGIN:VCARD, then at
//     once VERSION:4.0, then at least one property, then END:VCARD. Lines
//     end in CRLF (RFC 6350; belcard also takes a bare LF).
//   - line 281, TEXT-CHAR (FN is text): a backslash or a comma only in the
//     escapes "\\" and "\,"; a semicolon stands bare (belcard's grammar has
//     no "\;"); no control character. belcard_utils.cpp lines 77-82
//     unescape "\\" and "\," when the value is read.
//   - bctoolbox/src/utils/utils.cc line 73: lines are unfolded first (a
//     line break followed by a space or tab is removed), so a long line is
//     folded as RFC 6350 §3.2 says.
//   - liblinphone/src/vcard/vcard.cpp line 276: a friend's SIP addresses
//     are its IMPP values that start with sip: (and its display name the
//     FN).
//   - liblinphone/src/friend/friend-list.cpp, synchronizeFriendsFromServerVcard4
//     (line 401): the download must answer 200 with a body that parses
//     into at least one card; only then are the old contacts replaced. A
//     card without a UID is given one.

// vCardLineMax is the longest line RFC 6350 §3.2 lets a vCard hold, in
// octets, not counting its CRLF.
const vCardLineMax = 75

// contactsVCard renders the contacts: the extensions, then the streams,
// each sorted as its directory is (directoryXML), every name whole. sipHost
// is what a card's URI names after the "@": the PBX's address, with its SIP
// port unless that is 5060. No entries render nothing, and directoryFiles
// then writes no file: Linphone keeps the contacts it had after a 404 as
// after an empty body, and nothing would replace them anyway.
func contactsVCard(sipHost string, extensions, streams []directoryEntry) []byte {
	var b bytes.Buffer
	for _, list := range [][]directoryEntry{extensions, streams} {
		for _, e := range sortedEntries(list) {
			name := e.Name
			if strings.TrimSpace(name) == "" {
				name = e.Number
			}
			b.WriteString("BEGIN:VCARD\r\n")
			b.WriteString("VERSION:4.0\r\n")
			writeFolded(&b, "FN:"+vCardText(name))
			writeFolded(&b, "IMPP:sip:"+e.Number+"@"+sipHost)
			b.WriteString("END:VCARD\r\n")
		}
	}
	return b.Bytes()
}

// contactsSIPHost is contactsVCard's sipHost for the settings.
func contactsSIPHost(s Settings) string {
	if s.SIPPort == 0 || s.SIPPort == 5060 {
		return s.PBXAddress
	}
	return fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort)
}

// vCardText is s as a belcard text value: valid UTF-8, a control character
// as a space, "\" and "," escaped.
func vCardText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ',':
			b.WriteString(`\,`)
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeFolded writes line and its CRLF, folded (RFC 6350 §3.2) so that no
// line passes vCardLineMax octets: each continuation starts with a space,
// and no fold splits a character or an escape.
func writeFolded(b *bytes.Buffer, line string) {
	n := 0 // octets on the current line
	for i := 0; i < len(line); {
		_, size := utf8.DecodeRuneInString(line[i:])
		if line[i] == '\\' && i+1 < len(line) {
			size = 2 // keep an escape on one line
		}
		if n+size > vCardLineMax {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteString(line[i : i+size])
		n += size
		i += size
	}
	b.WriteString("\r\n")
}
