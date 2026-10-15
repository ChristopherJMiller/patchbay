package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/agent"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// agentTokenEnv is the environment variable the agent token can come from
// when --token-file is not given.
const agentTokenEnv = "PATCHBAY_AGENT_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential.

func newAgentCmd() *cobra.Command {
	var addr, tokenFile string
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the PBX agent: the sidecar that senses and acts, never decides",
		Long: `Run the agent, the native sidecar in the PBX pod. It serves the control API the
controller polls (GET /v1/observations, POST /v1/actions/...), and will apply
config bundles, serve TFTP/HTTP and watch phones.

Today it serves the control API skeleton only: health probes and an empty
observations document.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().StringVar(&addr, "listen", net.JoinHostPort("", strconv.Itoa(api.DefaultPort)),
		"control API address")
	cmd.Flags().StringVar(&tokenFile, "token-file", "",
		"file holding the control API bearer token (default $"+agentTokenEnv+")")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		log := ctrl.Log.WithName("agent")
		tokens, err := agentTokens(tokenFile, log)
		if err != nil {
			return err
		}
		h, err := agent.NewHandler(agent.Options{Tokens: tokens, Log: log})
		if err != nil {
			return err
		}
		log.Info("starting", "version", version.String())
		return agent.Serve(cmd.Context(), addr, h, log)
	}
	return cmd
}

// agentTokens returns the control API's token source: the token file,
// re-read whenever its Secret changes, or else the environment's fixed token.
func agentTokens(file string, log logr.Logger) (func() []string, error) {
	if file != "" {
		f, err := agent.NewTokenFile(file, agent.DefaultTokenGrace, nil, log.WithName("token"))
		if err != nil {
			return nil, fmt.Errorf("--token-file: %w", err)
		}
		return f.Tokens, nil
	}
	if tok := strings.TrimSpace(os.Getenv(agentTokenEnv)); tok != "" {
		return agent.StaticToken(tok), nil
	}
	return nil, errors.New("agent: no control API token: pass --token-file or set " + agentTokenEnv)
}
