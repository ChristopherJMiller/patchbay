package compile

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"fmt"
	"slices"
)

// Directory limits from Cisco's IP Phone Services SDK
// (CiscoIPPhoneDirectory).
const (
	// DirectoryMaxEntries is the most DirectoryEntry elements one
	// CiscoIPPhoneDirectory may hold; the rest are dropped.
	DirectoryMaxEntries = 32
	// directoryMaxName is the longest Name a DirectoryEntry holds.
	directoryMaxName = 32
)

// directoryEntry is one name and number in the phones' directory.
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

// directoryXML renders the CiscoIPPhoneDirectory every 79x5 shows under
// Directories (its directoryURL): one entry per routed device and person,
// sorted by number, names cut to 32 characters, at most
// DirectoryMaxEntries. It is served to anyone on device HTTP, so it holds
// names and numbers only, never a placeholder.
func directoryXML(entries []directoryEntry) ([]byte, error) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b directoryEntry) int {
		return cmp.Or(cmp.Compare(len(a.Number), len(b.Number)), cmp.Compare(a.Number, b.Number), cmp.Compare(a.Name, b.Name))
	})
	for i := range entries {
		if r := []rune(entries[i].Name); len(r) > directoryMaxName {
			entries[i].Name = string(r[:directoryMaxName])
		}
	}
	d := ciscoDirectory{Title: "patchbay", Entries: entries}
	if n := len(entries) - DirectoryMaxEntries; n > 0 {
		d.Entries = entries[:DirectoryMaxEntries]
		d.Dropped = xml.Comment(fmt.Sprintf(" %d more entries dropped: a CiscoIPPhoneDirectory holds at most %d ", n, DirectoryMaxEntries))
	}
	body, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("compile: directory XML: %w", err)
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.Write(body)
	b.WriteString("\n")
	return b.Bytes(), nil
}
