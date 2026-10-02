package openai_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// redirectTo answers every request with a redirect.
func redirectTo(status int, location string) http.HandlerFunc {
	return failWith(status, http.Header{"Location": {location}}, "")
}

var redirectCalls = map[string]func(ctx context.Context, c *openai.Client) error{
	"Generate": func(ctx context.Context, c *openai.Client) error {
		_, err := c.Generate(ctx, llm.Request{Messages: userMsg("hi")})
		return err
	},
	"Stream": func(ctx context.Context, c *openai.Client) error {
		_, err := c.Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
		return err
	},
	"Embed": func(ctx context.Context, c *openai.Client) error {
		_, err := c.Embed(ctx, llm.EmbedRequest{Model: "e", Input: []string{"a"}})
		return err
	},
}

// The default client does not follow a redirect. One would send the caller's
// headers, and for a 307 or 308 the whole prompt, to whatever host the answer
// named, and a 301 would turn the POST into a GET.
func TestClient_DefaultClientDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for name, call := range redirectCalls {
			t.Run(strconv.Itoa(status)+" "+name, func(t *testing.T) {
				elsewhere, elsewhereRec := newServer(t, serveJSON(simpleReply))
				srv, rec := newServer(t, redirectTo(status, elsewhere.URL+"/v1/chat/completions"))
				c := newClient(t, srv, func(o *openai.Options) {
					o.APIKey = "sk-test-123"
					o.EmbeddingModel = "e"
					o.Header = http.Header{"X-Gateway": {"secret"}}
				})

				err := call(t.Context(), c)

				got := asLLMError(t, err)
				assert.Equal(t, "openai", got.Provider)
				assert.Equal(t, status, got.Status)
				assert.False(t, got.Retryable, "the same request meets the same redirect")
				assert.Contains(t, got.Message, "redirect")
				assert.Contains(t, got.Message, "not followed")
				assert.Zero(t, elsewhereRec.count(), "nothing was sent to the host the redirect named")
				assert.Equal(t, 1, rec.count())
				assert.Equal(t, http.MethodPost, rec.only(t).Method)
			})
		}
	}
}

func TestClient_ARedirectWithNoLocationIsStillAnError(t *testing.T) {
	srv, _ := newServer(t, failWith(302, nil, ""))
	err := redirectCalls["Generate"](t.Context(), newClient(t, srv))
	got := asLLMError(t, err)
	assert.Equal(t, 302, got.Status)
	assert.False(t, got.Retryable)
}

// A client the caller supplies keeps its own policy on redirects. What the
// package adds is that neither a credential nor a header it was given is sent
// to a host other than the configured one, whatever the policy.
func TestClient_SuppliedClientKeepsItsRedirectPolicy(t *testing.T) {
	secrets := func(o *openai.Options) {
		o.APIKey = "sk-test-123"
		o.Header = http.Header{"X-Gateway": {"secret"}}
	}

	t.Run("a client that follows redirects follows them, and strips what must not travel", func(t *testing.T) {
		elsewhere, elsewhereRec := newServer(t, serveJSON(simpleReply))
		srv, _ := newServer(t, redirectTo(307, elsewhere.URL+"/v1/chat/completions"))
		c := newClient(t, srv, secrets, func(o *openai.Options) { o.HTTPClient = &http.Client{} })

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		require.NoError(t, err, "the caller's policy is to follow")
		got := elsewhereRec.only(t)
		assert.Empty(t, got.Header.Values("Authorization"), "the key went to another host")
		assert.Empty(t, got.Header.Values("X-Gateway"), "the caller's header went to another host")
		assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
	})

	t.Run("a redirect to the same host keeps them", func(t *testing.T) {
		srv, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/chat/completions" {
				redirectTo(307, "/v1/moved")(w, r)
				return
			}
			serveJSON(simpleReply)(w, r)
		})
		c := newClient(t, srv, secrets, func(o *openai.Options) { o.HTTPClient = &http.Client{} })

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		require.NoError(t, err)
		all := rec.all()
		require.Len(t, all, 2)
		assert.Equal(t, "/v1/moved", all[1].Path)
		assert.Equal(t, "Bearer sk-test-123", all[1].Header.Get("Authorization"))
		assert.Equal(t, "secret", all[1].Header.Get("X-Gateway"))
	})

	t.Run("a client's own redirect check still decides, and still runs", func(t *testing.T) {
		elsewhere, elsewhereRec := newServer(t, serveJSON(simpleReply))
		srv, _ := newServer(t, redirectTo(307, elsewhere.URL+"/v1/chat/completions"))
		var asked int
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			asked++
			return http.ErrUseLastResponse
		}}
		c := newClient(t, srv, secrets, func(o *openai.Options) { o.HTTPClient = hc })

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		got := asLLMError(t, err)
		assert.Equal(t, 307, got.Status)
		assert.Equal(t, 1, asked, "the caller's policy was asked")
		assert.Zero(t, elsewhereRec.count())
	})

	t.Run("a client's own redirect check that follows is still stripped of what must not travel", func(t *testing.T) {
		elsewhere, elsewhereRec := newServer(t, serveJSON(simpleReply))
		srv, _ := newServer(t, redirectTo(307, elsewhere.URL+"/v1/chat/completions"))
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
		c := newClient(t, srv, secrets, func(o *openai.Options) { o.HTTPClient = hc })

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		require.NoError(t, err)
		got := elsewhereRec.only(t)
		assert.Empty(t, got.Header.Values("Authorization"))
		assert.Empty(t, got.Header.Values("X-Gateway"))
	})

	t.Run("the caller's own client is left as it was", func(t *testing.T) {
		hc := &http.Client{}
		srv, _ := newServer(t, serveJSON(simpleReply))
		_ = newClient(t, srv, func(o *openai.Options) { o.HTTPClient = hc })
		assert.Nil(t, hc.CheckRedirect, "New does not edit the client it is given")
	})

	t.Run("a client with no policy of its own stops after ten redirects, as net/http does", func(t *testing.T) {
		var hops int
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			hops++
			redirectTo(307, "/v1/chat/completions")(w, r)
		})
		c := newClient(t, srv, func(o *openai.Options) { o.HTTPClient = &http.Client{} })

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "stopped after 10 redirects")
		assert.Equal(t, 10, hops, "ten requests, the tenth redirect refused")
	})
}
