package cli_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

func TestAgentConnRedactsToken(t *testing.T) {
	const token = "not-a-secret-agent-token"
	c := cli.AgentConn{BaseURL: "http://127.0.0.1:1", Token: token, Close: func() error { return nil }}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{c, &c} {
			if got := fmt.Sprintf(format, v); strings.Contains(got, token) {
				t.Errorf("%s printed the token: %s", format, got)
			}
		}
	}
}

// The CLI finds the PBX pod with the labels the controller's objects and
// the chart use.
func TestLabelsMatchInstall(t *testing.T) {
	if cli.LabelPartOf != install.LabelPartOf || cli.PartOf != install.PartOf {
		t.Errorf("part-of %s=%s, install has %s=%s", cli.LabelPartOf, cli.PartOf, install.LabelPartOf, install.PartOf)
	}
}
