package mcp

// What a saved oauth credential is WORTH, asked without spending it. A run asks on its way to building a transport (oauthTokenSource); the web UI's mcp tab asks so somebody can see why an agent's tools are failing without starting a job. One function answers both, because a page that said "connected" where a run says "authorized for a different endpoint" would be worse than a page with no status at all.

import (
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// TokenState is where one oauth server's saved credential stands. It carries no part of the credential itself — a page renders this, and the token is the one thing that must never reach one.
type TokenState struct {
	// Connected is a token that can be used right now, whether or not it can renew itself.
	Connected bool
	// Renews is a refresh token in hand, which is what makes a credential survive unattended.
	Renews bool
	// Detail is the sentence a person acts on: what is wrong, or that nothing is.
	Detail string
	// Path is where the token lives, so an operator can find, inspect or delete it.
	Path string
}

// InspectToken reports the state of srv's saved credential from the file alone: no request, no refresh, nothing spent. The checks and their order are checkCredential's, which is what a run goes through, so the page and the run cannot disagree — but the WORDING is not, because a run's error names the login command to go and type and a page has a button for that.
func InspectToken(pipeline string, srv config.MCPServer) TokenState {
	tf, path, fault, _ := checkCredential(pipeline, srv)
	state := TokenState{Path: path}

	// The absent token file IS the unusable state, so this is the guard rather than the fault being one: a fault and a token that agree only by invariant are two values nothing checks.
	if tf == nil {
		state.Detail = faultDetail(fault)

		return state
	}

	state.Connected = true
	state.Renews = tf.RefreshToken != ""

	switch {
	case state.Renews:
		state.Detail = "connected, renews automatically"
	case tf.Expiry.IsZero():
		state.Detail = "connected"
	default:
		// A token with no refresh token works until it does not, and then needs a person. Saying when is the only warning anybody gets.
		state.Detail = "connected, but it cannot renew itself — expires " + tf.Expiry.Format(time.RFC3339)
	}

	return state
}

// credentialFault is WHICH of the unusable states a token file is in, so a caller can word it for its own reader without parsing the error it also gets.
type credentialFault int

const (
	faultNone credentialFault = iota
	faultNoToken
	faultWrongEndpoint
	faultExpired
)

// faultDetail is the phrase a page puts on a credential that cannot be used, where a run's error names the login command to go and type instead.
func faultDetail(fault credentialFault) string {
	switch fault {
	case faultWrongEndpoint:
		return "authorized for a different endpoint — needs login"
	case faultExpired:
		return "expired, with no refresh token — needs login"
	case faultNoToken, faultNone:
		return "needs login"
	default:
		return "needs login"
	}
}

// checkCredential loads srv's token file and refuses the three states that make it unusable, in the order a run meets them: no file, a file authorized for a different endpoint, and an access token that expired with nothing to renew it.
func checkCredential(pipeline string, srv config.MCPServer) (*TokenFile, string, credentialFault, error) {
	path, err := TokenPath(pipeline, srv.Name)
	if err != nil {
		return nil, "", faultNoToken, err
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		return nil, path, faultNoToken, fmt.Errorf("mcp server %q is not authorized for pipeline %q (%w %s with -p %s --target <url> for a daemon, or -c <pipeline.yml> here, adding --name %s=<pipeline.yml> if that is not its file name): %w", srv.Name, pipeline, ErrNeedsLogin, srv.Name, pipeline, pipeline, err)
	}

	if tf.Endpoint != srv.Endpoint {
		return nil, path, faultWrongEndpoint, fmt.Errorf("mcp server %q in pipeline %q: authorized for a different endpoint (%w %s with -p %s again)", srv.Name, pipeline, ErrNeedsLogin, srv.Name, pipeline)
	}

	// Caught here rather than left to x/oauth2, which answers this exact state with a bare "token expired and refresh token is not set" — true, but it names neither the server nor the fix, and it arrives only after the transport has already been built. The state is knowable from the file alone, so it is answered from the file alone.
	err = tf.checkRefreshable()
	if err != nil {
		return nil, path, faultExpired, fmt.Errorf("mcp server %q: %w", srv.Name, err)
	}

	return tf, path, faultNone, nil
}
