package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// amiResponse is one AMI response block's fields.
type amiResponse map[string]string

// amiClient is the least AMI client that can log in and send actions: the
// e2e suite uses it to show what the agent's account can and cannot do.
type amiClient struct {
	conn net.Conn
	r    *bufio.Reader
	next int // the next ActionID
}

// dialAMI connects, reads the banner and logs in with events off.
func dialAMI(ctx context.Context, addr, user, secret string) (*amiClient, string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("ami: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c := &amiClient{conn: conn, r: bufio.NewReader(conn), next: 1}
	banner, err := c.r.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, "", fmt.Errorf("ami: reading the banner: %w", err)
	}
	res, err := c.do("Login", [][2]string{{"Username", user}, {"Secret", secret}, {"Events", "off"}})
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	if res["Response"] != "Success" {
		_ = conn.Close()
		return nil, "", fmt.Errorf("ami: login refused: %s", res["Message"])
	}
	return c, strings.TrimSpace(banner), nil
}

func (c *amiClient) close() { _ = c.conn.Close() }

// do sends one action and returns its response, skipping anything without
// its ActionID (events).
func (c *amiClient) do(action string, fields [][2]string) (amiResponse, error) {
	id := strconv.Itoa(c.next)
	c.next++
	var b strings.Builder
	b.WriteString("Action: " + action + "\r\nActionID: " + id + "\r\n")
	for _, f := range fields {
		if strings.ContainsAny(f[0]+f[1], "\r\n") {
			return nil, fmt.Errorf("ami: %s: a field holds a line break", action)
		}
		b.WriteString(f[0] + ": " + f[1] + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, fmt.Errorf("ami: %s: %w", action, err)
	}
	for {
		block, err := c.readBlock()
		if err != nil {
			return nil, fmt.Errorf("ami: %s: %w", action, err)
		}
		if block["ActionID"] == id {
			return block, nil
		}
	}
}

// readBlock reads "Key: value" lines up to a blank line.
func (c *amiClient) readBlock() (amiResponse, error) {
	block := amiResponse{}
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(block) == 0 {
				continue
			}
			return block, nil
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			if _, dup := block[k]; !dup {
				block[k] = strings.TrimSpace(v)
			}
		}
	}
}

// parseAMIAction reads "Action Key=Value Key=Value" into an action name and
// its fields.
func parseAMIAction(s string) (string, [][2]string, error) {
	words := strings.Fields(s)
	if len(words) == 0 {
		return "", nil, errors.New("an empty action")
	}
	var fields [][2]string
	for _, w := range words[1:] {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return "", nil, fmt.Errorf("action %q: %q is not Key=Value", s, w)
		}
		fields = append(fields, [2]string{k, v})
	}
	return words[0], fields, nil
}

func newAMICmd(rf *rootFlags) *cobra.Command {
	var addr, user string
	cmd := &cobra.Command{
		Use:   `ami "<Action> [Key=Value]..."...`,
		Short: "Log in to AMI and send each action; print every response",
		Long: `Log in to Asterisk's manager interface (AMI binds 127.0.0.1, so the pod
must be on the PBX node's host network) and send each argument as one action,
"ModuleLoad Module=app_system.so LoadType=load" for example. Each response is
printed as one line: "action=<n> <Action> response=<Response> message=<Message>".
Exit 0 when the login worked and every action was answered, whatever the
answers were: the caller judges them.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := rf.logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), rf.timeout)
			defer cancel()
			// The secret only from the environment (a Secret), never a flag.
			secret := env("AMI_SECRET", "")
			if secret == "" {
				return errors.New("no AMI secret: set $FAKEPHONE_AMI_SECRET")
			}
			c, banner, err := dialAMI(ctx, addr, user, secret)
			if err != nil {
				return err
			}
			defer c.close()
			log.Info("logged in", "addr", addr, "user", user, "banner", banner)
			for i, a := range args {
				action, fields, err := parseAMIAction(a)
				if err != nil {
					return err
				}
				res, err := c.do(action, fields)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "action=%d %s response=%s message=%q\n",
					i, a, res["Response"], res["Message"])
			}
			_, _ = c.do("Logoff", nil)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", env("AMI_ADDR", "127.0.0.1:5038"), "AMI's address ($FAKEPHONE_AMI_ADDR)")
	f.StringVar(&user, "user", env("AMI_USER", "patchbay-agent"), "AMI username ($FAKEPHONE_AMI_USER)")
	return cmd
}
