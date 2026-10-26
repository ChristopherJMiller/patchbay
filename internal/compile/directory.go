package compile

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"fmt"
	"slices"
	"strings"
)

// The phones' directory and Services menu: the http/ files a 79x5's
// Directories and Services buttons open (layout.go names them).
//
// A 79x5 adds to its Directories menu, after its own Missed, Received and
// Placed Calls, the items of the CiscoIPPhoneMenu its directoryURL
// returns; a CiscoIPPhoneDirectory there is fetched and never shown (the
// Desk 7945 on SIP 9.4(2)SR4-3, 2026-10-25). Its Services button shows the
// object its servicesURL returns. So directory.xml and services.xml are one
// menu, titled patchbay:
//
//	Extensions -> directory/extensions.xml  every routed Phone and Softphone, every Person ringing one
//	Streams    -> directory/streams.xml     every Stream whose number dials the bridge
//
// Each list is a CiscoIPPhoneDirectory: pick an entry and the phone dials
// it. The Streams item and its file exist only while such a Stream does
// (a Stream with a refused source, or on an install without a bridge,
// plays "not available": it is not offered). Every MenuItem URL is
// absolute, http://<pbxAddress>:<deviceHTTPPort>/..., as Cisco's SDK
// writes them, so nothing rests on how a phone resolves a relative one; an
// IPv4 address and a port keep it to 53 characters, under the SDK's 256.
//
// All of it is served to anyone on device HTTP, so it holds names, numbers
// and the PBX's own URLs, never a placeholder.

// Limits from Cisco's IP Phone Services SDK (CiscoIPPhoneDirectory).
const (
	// DirectoryMaxEntries is the most DirectoryEntry elements one
	// CiscoIPPhoneDirectory may hold; the rest are dropped.
	DirectoryMaxEntries = 32
	// directoryMaxName is the longest Name a DirectoryEntry holds.
	directoryMaxName = 32
)

// The menu's title, and its items' names, which are also the titles of the
// directories they open (each well under the SDK's 32 characters).
const (
	menuTitle       = "patchbay"
	extensionsTitle = "Extensions"
	streamsTitle    = "Streams"
)

// directoryEntry is one name and number in a directory.
type directoryEntry struct {
	Name   string `xml:"Name"`
	Number string `xml:"Telephone"`
}

type ciscoDirectory struct {
	XMLName xml.Name         `xml:"CiscoIPPhoneDirectory"`
	Title   string           `xml:"Title"`
	Entries []directoryEntry `xml:"DirectoryEntry"`
	Dropped xml.Comment      `xml:",comment"`
}

type ciscoMenu struct {
	XMLName xml.Name   `xml:"CiscoIPPhoneMenu"`
	Title   string     `xml:"Title"`
	Items   []menuItem `xml:"MenuItem"`
}

type menuItem struct {
	Name string `xml:"Name"`
	URL  string `xml:"URL"`
}

// directoryFiles renders the whole http/ tree: the menu at DirectoryFile
// and ServicesFile, ExtensionsFile, StreamsFile when streams is not empty,
// and the softphones' ContactsFile (contacts.go) when either list is not.
// base is device HTTP's URL
// (deviceHTTPBase) and sipHost the contacts' (contactsSIPHost); extensions
// are the routed devices' and the people's entries, streams those of the
// Streams that dial the bridge.
func directoryFiles(base, sipHost string, extensions, streams []directoryEntry) (map[string][]byte, error) {
	files := map[string][]byte{}
	menu := ciscoMenu{Title: menuTitle}
	add := func(title, file string, entries []directoryEntry) error {
		doc, err := directoryXML(title, entries)
		if err != nil {
			return err
		}
		files[file] = doc
		menu.Items = append(menu.Items, menuItem{Name: title, URL: base + deviceHTTPPath(file)})
		return nil
	}
	if err := add(extensionsTitle, ExtensionsFile, extensions); err != nil {
		return nil, err
	}
	if len(streams) > 0 {
		if err := add(streamsTitle, StreamsFile, streams); err != nil {
			return nil, err
		}
	}
	doc, err := xmlDocument(menu)
	if err != nil {
		return nil, fmt.Errorf("compile: directory menu XML: %w", err)
	}
	files[DirectoryFile], files[ServicesFile] = doc, bytes.Clone(doc)
	if contacts := contactsVCard(sipHost, extensions, streams); len(contacts) > 0 {
		files[ContactsFile] = contacts
	}
	return files, nil
}

// sortedEntries is a copy of entries sorted by number (shorter numbers
// first), then name.
func sortedEntries(entries []directoryEntry) []directoryEntry {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b directoryEntry) int {
		return cmp.Or(cmp.Compare(len(a.Number), len(b.Number)), cmp.Compare(a.Number, b.Number), cmp.Compare(a.Name, b.Name))
	})
	return entries
}

// directoryXML renders one CiscoIPPhoneDirectory: its entries sorted by
// number (sortedEntries), names cut to 32 characters, at most
// DirectoryMaxEntries, with a comment counting the rest.
func directoryXML(title string, entries []directoryEntry) ([]byte, error) {
	entries = sortedEntries(entries)
	for i := range entries {
		if r := []rune(entries[i].Name); len(r) > directoryMaxName {
			entries[i].Name = string(r[:directoryMaxName])
		}
	}
	d := ciscoDirectory{Title: title, Entries: entries}
	if n := len(entries) - DirectoryMaxEntries; n > 0 {
		d.Entries = entries[:DirectoryMaxEntries]
		d.Dropped = xml.Comment(fmt.Sprintf(" %d more entries dropped: a CiscoIPPhoneDirectory holds at most %d ", n, DirectoryMaxEntries))
	}
	doc, err := xmlDocument(d)
	if err != nil {
		return nil, fmt.Errorf("compile: directory XML: %w", err)
	}
	return doc, nil
}

// xmlDocument is v indented, after the UTF-8 XML declaration, with a final
// newline. encoding/xml escapes all text.
func xmlDocument(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.Write(body)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// deviceHTTPBase is device HTTP's URL, http://<pbxAddress>:<deviceHTTPPort>.
func deviceHTTPBase(s Settings) string {
	return fmt.Sprintf("http://%s:%d", s.PBXAddress, s.DeviceHTTPPort)
}

// deviceHTTPPath is the URL path device HTTP serves a file of http/ at.
func deviceHTTPPath(file string) string {
	return strings.TrimPrefix(file, TreeHTTP)
}
