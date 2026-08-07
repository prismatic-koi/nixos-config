package cmd

// sidecar_titlegen.go — builds the once-per-session title generator the
// sidecar uses (issue #2683).
//
// The sidecar runs HOST-SIDE, which is what makes this possible at all:
// ~/.config/prism/accounts/ and pi's auth.json are not visible inside an
// agent sandbox (see cmd/account_usage_refresh.go's refreshUnavailable), but
// the sidecar process itself is outside one.

import (
	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/titlegen"
	"github.com/prismatic-koi/prism/internal/usage"
)

// newTitleGenerator returns the generator the sidecar should use, or nil to
// disable the model call.
//
// It is a package-level variable so tests can substitute a stub. The seam is
// deliberately IN-PROCESS rather than an environment variable, for the same
// reason internal/usage documents: the request carries a long-lived OAuth
// bearer token, and an env-controlled destination would let a `.envrc` or a
// stray export redirect that credential to a host of its choosing.
//
// Returning nil is a supported, silent outcome. A host with no Anthropic
// credentials — a fresh machine, an account that has never run
// `prism account login` — still gets fallback titles and issue references;
// it just does not get model-written summaries. Refusing to start a sidecar
// over a missing display string would be absurd, so nothing here is fatal.
var newTitleGenerator = func() sidecar.TitleGenerator {
	paths, err := account.ResolvePaths()
	if err != nil {
		return nil
	}
	return &titlegen.Generator{
		BaseURL: usage.DefaultBaseURL,
		// Resolved per call, never cached. The sidecar outlives the access
		// token: pi rotates it in auth.json roughly every ten hours, and a
		// copy captured at sidecar start would be stale — and answering 401
		// — long before a long-lived coordinator session ends.
		Token: func() (string, error) {
			creds, err := account.ReadLiveCredentials(paths)
			if err != nil {
				return "", err
			}
			return creds.Access, nil
		},
	}
}
