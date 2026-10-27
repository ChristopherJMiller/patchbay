// Package messages is phone messages in the controller
// (docs/superpowers/specs/2026-10-26-phone-messages-design.md), in three
// parts:
//
//   - Server, the messages endpoint: the small HTTP server Home Assistant
//     POSTs phone messages to and PUTs the house state to (wire contract:
//     package messagesapi, internal/controller/messages/api). It creates
//     Message resources, one per target phone, and records the house state
//     (quiet, away) in the <fullname>-house ConfigMap; it decides and
//     delivers nothing. Every replica serves (NeedLeaderElection is false):
//     the writes are creates and optimistic-concurrency updates, safe from
//     any replica. The bearer token is read from a Secret through the
//     manager's cached client on every request, so a rotated token works
//     without a restart; it is never logged, echoed or put in an error.
//   - Decide (decide.go, verdict.go), pure: every Message's status (Held
//     with a reason, Shown, Expired, Withdrawn, Replaced, Failed) and every
//     phone's screen (a page, with the chime for a Message never shown, or
//     a clear) from the Messages, the house state and a PhoneView of each
//     Phone. An ordered rule table, tested row by row.
//   - Reconciler (reconciler.go), the leader-only shell: it builds Decide's
//     input from the cache, the agent's snapshot and its memory of its own
//     pushes, sends the pushes through the agent (agentview.Actor.Push),
//     and writes what Decide returns: statuses, Events and the deletion of
//     finished Messages a day old.
package messages

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
)

// Limits, the agent control API's shape.
const (
	// MaxBody is the largest request body.
	MaxBody = 8 << 10
	// MaxConns and MaxConnsPerSource are the connection caps (netlimit).
	MaxConns          = 64
	MaxConnsPerSource = 16

	readHeaderTimeout = 5 * time.Second
	ioTimeout         = 30 * time.Second
	idleTimeout       = 15 * time.Second
	shutdownTimeout   = 10 * time.Second

	// errLimit bounds an API error's text in an answer.
	errLimit = 200
)

// ServerConfig configures the endpoint.
type ServerConfig struct {
	// Client reads and writes Messages, the house ConfigMap and the token
	// Secret; the manager's cached client.
	Client    client.Client
	Namespace string
	Names     install.Names
	// TokenSecret and TokenKey locate the bearer token.
	TokenSecret types.NamespacedName
	TokenKey    string
	// Addr is the listen address.
	Addr string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	Log logr.Logger
}

// Server is the endpoint. It is a manager.Runnable.
type Server struct {
	cfg ServerConfig
}

// NewServer checks cfg and returns the server.
func NewServer(cfg ServerConfig) (*Server, error) {
	var errs []error
	if cfg.Client == nil {
		errs = append(errs, errors.New("no client"))
	}
	if cfg.Namespace == "" {
		errs = append(errs, errors.New("no namespace"))
	}
	if cfg.Names.HouseConfigMap == "" {
		errs = append(errs, errors.New("no names"))
	}
	if cfg.TokenSecret.Name == "" || cfg.TokenKey == "" {
		errs = append(errs, errors.New("the token Secret needs a name and a key"))
	}
	if cfg.Addr == "" {
		errs = append(errs, errors.New("no listen address"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg}, nil
}

// NeedLeaderElection is false: every replica serves.
func (s *Server) NeedLeaderElection() bool { return false }

// Handler is the endpoint's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+messagesapi.PathMessages, s.auth(http.HandlerFunc(s.postMessages)))
	mux.Handle("POST "+messagesapi.PathWithdraw, s.auth(http.HandlerFunc(s.postWithdraw)))
	mux.Handle("GET "+messagesapi.PathHouse, s.auth(http.HandlerFunc(s.getHouse)))
	mux.Handle("PUT "+messagesapi.PathHouse, s.auth(http.HandlerFunc(s.putHouse)))
	return mux
}

// Start listens on the configured address and serves until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("messages: listen on %s: %w", s.cfg.Addr, err)
	}
	return s.serveOn(ctx, ln)
}

func (s *Server) serveOn(ctx context.Context, ln net.Listener) error {
	log := s.cfg.Log
	ln = netlimit.New(ln, MaxConns, MaxConnsPerSource, log)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       ioTimeout,
		WriteTimeout:      ioTimeout,
		IdleTimeout:       idleTimeout,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("messages endpoint listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("messages: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("messages: shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("messages: %w", err)
	}
	return nil
}

// auth requires the bearer token in the Secret now, white space around it
// trimmed (a value written with `echo` or a heredoc ends in a newline, and
// the agent's token is read the same way). No Secret or key, or only white
// space, is a 503 (nothing is written), a missing or wrong token a 401.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sec corev1.Secret
		err := s.cfg.Client.Get(r.Context(), s.cfg.TokenSecret, &sec)
		token := bytes.TrimSpace(sec.Data[s.cfg.TokenKey])
		if apierrors.IsNotFound(err) || (err == nil && len(token) == 0) {
			writeError(w, http.StatusServiceUnavailable, "no messages token configured")
			return
		}
		if err != nil {
			s.cfg.Log.Error(err, "reading the token Secret")
			writeError(w, http.StatusInternalServerError, "reading the token failed")
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), token) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="patchbay-messages"`)
			writeError(w, http.StatusUnauthorized, "missing or wrong messages token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// decode reads the body strictly into v, answering 413 or 400 itself.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		// Nothing but whitespace may follow the object.
		_, terr := dec.Token()
		var big *http.MaxBytesError
		switch {
		case errors.Is(terr, io.EOF):
		case errors.As(terr, &big):
			err = terr // too big
		default:
			err = errors.New("unexpected data after the object")
		}
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body over %d bytes", MaxBody))
		return false
	}
	writeError(w, http.StatusBadRequest, "bad request body: "+install.Text(err.Error(), errLimit))
	return false
}

func (s *Server) postMessages(w http.ResponseWriter, r *http.Request) {
	var req messagesapi.MessageRequest
	if !decode(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, install.Text(err.Error(), errLimit))
		return
	}
	expires, _ := req.ExpiresIn() // Validate checked it
	expiresAt := metav1.NewTime(s.cfg.Now().Add(expires))
	var names []string
	for _, phone := range req.To {
		msg := &v1alpha1.Message{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "msg-",
				Namespace:    s.cfg.Namespace,
				Labels:       install.ManagedLabels(),
			},
			Spec: v1alpha1.MessageSpec{
				Phone: v1alpha1.LocalName(phone), Title: req.Title, Text: req.Text,
				Urgent: req.Urgent, ExpiresAt: expiresAt, Key: req.Key,
			},
		}
		if err := s.cfg.Client.Create(r.Context(), msg); err != nil {
			s.cfg.Log.Error(err, "creating a Message", "phone", phone)
			writeError(w, http.StatusInternalServerError, "creating the message failed: "+install.Text(err.Error(), errLimit))
			return
		}
		names = append(names, msg.Name)
	}
	writeJSON(w, http.StatusCreated, messagesapi.MessageResponse{Names: names})
}

// final reports a state a Message never leaves.
func final(st v1alpha1.MessageState) bool {
	return st == v1alpha1.MessageStateExpired || st == v1alpha1.MessageStateWithdrawn ||
		st == v1alpha1.MessageStateReplaced || st == v1alpha1.MessageStateFailed
}

func (s *Server) postWithdraw(w http.ResponseWriter, r *http.Request) {
	var req messagesapi.WithdrawRequest
	if !decode(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, install.Text(err.Error(), errLimit))
		return
	}
	ctx := r.Context()
	var list v1alpha1.MessageList
	if err := s.cfg.Client.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		s.cfg.Log.Error(err, "listing Messages")
		writeError(w, http.StatusInternalServerError, "listing messages failed: "+install.Text(err.Error(), errLimit))
		return
	}
	n := 0
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.Key != req.Key || final(m.Status.State) {
			continue
		}
		if len(req.To) > 0 && !slices.Contains(req.To, string(m.Spec.Phone)) {
			continue
		}
		set := false
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var cur v1alpha1.Message
			if err := s.cfg.Client.Get(ctx, client.ObjectKeyFromObject(m), &cur); err != nil {
				return err
			}
			if cur.Spec.Withdrawn || final(cur.Status.State) {
				set = false
				return nil
			}
			cur.Spec.Withdrawn = true
			set = true
			return s.cfg.Client.Update(ctx, &cur)
		})
		if apierrors.IsNotFound(err) {
			continue // gone since the list
		}
		if err != nil {
			s.cfg.Log.Error(err, "withdrawing a Message", "message", m.Name)
			writeError(w, http.StatusInternalServerError, "withdrawing failed: "+install.Text(err.Error(), errLimit))
			return
		}
		if set {
			n++
		}
	}
	writeJSON(w, http.StatusOK, messagesapi.WithdrawResponse{Withdrawn: n})
}

// readHouse returns the stored House and the ConfigMap ("" data and a nil
// map when it does not exist).
func (s *Server) readHouse(ctx context.Context) (messagesapi.House, *corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: s.cfg.Namespace, Name: s.cfg.Names.HouseConfigMap}
	if err := s.cfg.Client.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return messagesapi.House{}, nil, nil
		}
		return messagesapi.House{}, nil, err
	}
	raw, ok := cm.Data[install.HouseKey]
	if !ok || raw == "" {
		return messagesapi.House{}, &cm, nil
	}
	h, err := messagesapi.ParseHouse([]byte(raw))
	if err != nil {
		return messagesapi.House{}, &cm, err
	}
	return h, &cm, nil
}

func (s *Server) getHouse(w http.ResponseWriter, r *http.Request) {
	h, _, err := s.readHouse(r.Context())
	if err != nil {
		s.cfg.Log.Error(err, "reading the house ConfigMap")
		writeError(w, http.StatusInternalServerError, "reading the house state failed: "+install.Text(err.Error(), errLimit))
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) putHouse(w http.ResponseWriter, r *http.Request) {
	var upd messagesapi.HouseUpdate
	if !decode(w, r, &upd) {
		return
	}
	if err := upd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, install.Text(err.Error(), errLimit))
		return
	}
	var out messagesapi.House
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		h, cm, err := s.readHouse(r.Context())
		if err != nil {
			return err
		}
		if upd.Quiet != nil {
			h.Quiet = *upd.Quiet
		}
		if upd.Away != nil {
			h.Away = *upd.Away
		}
		h.UpdatedAt = s.cfg.Now().UTC()
		raw, err := json.Marshal(h)
		if err != nil {
			return err
		}
		out = h
		if cm == nil {
			err := s.cfg.Client.Create(r.Context(), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: s.cfg.Names.HouseConfigMap, Namespace: s.cfg.Namespace, Labels: install.ManagedLabels(),
				},
				Data: map[string]string{install.HouseKey: string(raw)},
			})
			if apierrors.IsAlreadyExists(err) {
				// Lost a create race: retry as an update.
				return apierrors.NewConflict(corev1.Resource("configmaps"), s.cfg.Names.HouseConfigMap, err)
			}
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[install.HouseKey] = string(raw)
		return s.cfg.Client.Update(r.Context(), cm)
	})
	// A lost create race is a conflict too: the next try reads and updates.
	if err != nil {
		s.cfg.Log.Error(err, "writing the house ConfigMap")
		writeError(w, http.StatusInternalServerError, "writing the house state failed: "+install.Text(err.Error(), errLimit))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, messagesapi.Error{Error: msg})
}
