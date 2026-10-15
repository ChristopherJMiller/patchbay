package compile_test

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// confSection is one [section] of an Asterisk config file as Asterisk's
// own parser (main/config.c) would read it.
type confSection struct {
	Name    string
	Options []confOption
}

type confOption struct {
	Key, Value string
	Arrow      bool // written "key => value"
}

// get returns every value of key, in order.
func (s confSection) get(key string) []string {
	var out []string
	for _, o := range s.Options {
		if o.Key == key {
			out = append(out, o.Value)
		}
	}
	return out
}

// one returns key's only value, and false unless there is exactly one.
func (s confSection) one(key string) (string, bool) {
	v := s.get(key)
	if len(v) != 1 {
		return "", false
	}
	return v[0], true
}

// parseConf is a minimal Asterisk config tokenizer: "[section]" headers,
// "key = value" and "key => value" lines, ";" comments to the end of the
// line, and "\;" for a literal semicolon. It refuses what the compiler never
// writes (block comments, #include, templates, a line outside a section),
// so a test notices if the compiler starts to.
func parseConf(content []byte) ([]confSection, error) {
	var sections []confSection
	sc := bufio.NewScanner(bytes.NewReader(content))
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		line, err := stripComment(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d %q: %w", n, raw, err)
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("line %d %q: a directive", n, raw)
		case strings.HasPrefix(line, "["):
			name, ok := strings.CutSuffix(line[1:], "]")
			if !ok || strings.ContainsAny(name, "[]()") {
				return nil, fmt.Errorf("line %d %q: not a plain section header", n, raw)
			}
			sections = append(sections, confSection{Name: name})
			continue
		}
		if len(sections) == 0 {
			return nil, fmt.Errorf("line %d %q: outside any section", n, raw)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d %q: no '='", n, raw)
		}
		arrow := strings.HasPrefix(value, ">")
		value = strings.TrimPrefix(value, ">")
		o := confOption{Key: strings.TrimSpace(key), Value: strings.ReplaceAll(strings.TrimSpace(value), `\;`, ";"), Arrow: arrow}
		sections[len(sections)-1].Options = append(sections[len(sections)-1].Options, o)
	}
	return sections, sc.Err()
}

// stripComment cuts the line at its first unescaped ";".
func stripComment(line string) (string, error) {
	for i := range len(line) {
		if line[i] != ';' || (i > 0 && line[i-1] == '\\') {
			continue
		}
		if strings.HasPrefix(line[i:], ";--") {
			return "", fmt.Errorf("a block comment")
		}
		return line[:i], nil
	}
	return line, nil
}

// pjsipObjects indexes pjsip.conf by (type, name). It fails on a section
// without exactly one type, or two objects of one type and name.
func pjsipObjects(content []byte) (map[[2]string]confSection, error) {
	sections, err := parseConf(content)
	if err != nil {
		return nil, err
	}
	out := map[[2]string]confSection{}
	for _, s := range sections {
		typ, ok := s.one("type")
		if !ok {
			return nil, fmt.Errorf("[%s] has %d type lines", s.Name, len(s.get("type")))
		}
		k := [2]string{typ, s.Name}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("two %s objects named %s", typ, s.Name)
		}
		out[k] = s
	}
	return out, nil
}
