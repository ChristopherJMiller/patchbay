// Package cli is the client half of the patchbay binary (design §6, CLI):
// status, softphone qr, phone resync, firmware load and models. Each
// command is a function over Env, the command's only way out of the process
// (the Kubernetes API, the agent's control API through a port-forward, the
// clock, the terminal), so every test runs without a cluster. cmd/patchbay
// only parses flags, builds the real Env (NewEnv) and calls the function.
//
// docs/plans/release-a.md §6.2 (W3.9) is the specification.
package cli
