package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The browser UI is form-encoded HTML over the same store the JSON API uses,
// so these tests prove the two agree: a note written through the form is
// listed, read and searched through the JSON API, and only by its owner.

// sampleNote is the note every template executes against in the parse test,
// carrying markup so a template that forgets to escape fails loudly.
func sampleNote() Note {
	return Note{
		ID:        "00000000-0000-0000-0000-000000000001",
		UserID:    "00000000-0000-0000-0000-000000000002",
		Title:     "Roof <script>alert(1)</script> repair",
		Body:      "Slate tiles, <b>south</b> side",
		CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

// TestTemplatesParseAndExecuteEveryBlock parses the embedded templates the way
// run() does and executes every block with hostile input, so a template that
// fails to parse or that renders markup unescaped goes red without a database.
func TestTemplatesParseAndExecuteEveryBlock(t *testing.T) {
	tmpl, err := ParseTemplates()
	require.NoError(t, err)

	note := sampleNote()
	page := notesPageData{
		Notes:      []Note{note},
		Flash:      "A title is required.",
		TitleError: "A title is required.",
		Title:      "kept <input>",
		Body:       "kept <textarea>",
	}

	for name, dot := range map[string]any{
		"notes":       page,
		"note_form":   page,
		"note_card":   note,
		"note_list":   page,
		"flash":       page,
		"empty_state": page,
		"pagination":  page,
	} {
		var buf bytes.Buffer
		require.NoError(t, tmpl.ExecuteTemplate(&buf, name, dot), "block %q failed to execute", name)
		out := buf.String()
		assert.NotContains(t, out, "<script>alert(1)", "block %q rendered markup unescaped", name)
		assert.NotContains(t, out, "<b>south</b>", "block %q rendered markup unescaped", name)
	}

	var formBuf bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&formBuf, "note_form", page))
	assert.Contains(t, formBuf.String(), "field-error", "a validation error must mark the field, not only flash")
	assert.Contains(t, formBuf.String(), `aria-invalid="true"`, "the invalid field must say so to assistive tech")

	var pageBuf bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&pageBuf, "notes", page))
	full := pageBuf.String()
	assert.Contains(t, full, "&lt;script&gt;", "the page must escape the note title")
	assert.Contains(t, full, `/static/vendor/htmx.min.js`, "the page must load the vendored htmx")
	assert.Contains(t, full, `/static/css/tokens.css`, "the page must load the token stylesheet")
	assert.Contains(t, full, `data-theme="dashboard"`, "the notes page is the dashboard theme")

	var fragBuf bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&fragBuf, "note_list", notesPageData{}))
	assert.Contains(t, fragBuf.String(), "No notes yet", "an empty list must render the empty state")
}

// postForm sends a form-encoded request, never following redirects: a 303 back
// to the list is an assertion about the response, not a page to fetch.
// postForm always targets the notes form: every UI write in these tests
// goes through /notes, so the path is fixed rather than a parameter.
func postForm(t *testing.T, ts *testService, token string, values url.Values, hx bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.srv.URL+"/notes", strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// getHX is get with an HX-Request header, which is what asks a UI route for
// its fragment instead of its page.
// getHX always reads the notes fragment: the only HX-Request surface under test.
func getHX(t *testing.T, ts *testService, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.srv.URL+"/notes", nil)
	require.NoError(t, err)
	req.Header.Set("HX-Request", "true")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

// deleteNote sends a DELETE the way htmx does, with HX-Request set when hx is
// true. Redirects are never followed, for the same reason as in postForm.
func deleteNote(t *testing.T, ts *testService, path, token string, hx bool) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, ts.srv.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp
}

func TestNotesPageRequiresAuth(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := get(t, ts, "/notes", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = getHX(t, ts, "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestNotesPageServesFormAndList(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "browser@example.com").Token

	_, raw := postNote(t, ts, token, `{"title":"Roof repair","body":"Slate tiles"}`)
	require.Contains(t, string(raw), "Roof repair")

	resp, raw := get(t, ts, "/notes", token)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body := string(raw)
	assert.Contains(t, body, "<form", "the page must carry the note form")
	assert.Contains(t, body, `hx-post="/notes"`, "the form must submit over htmx")
	assert.Contains(t, body, "Roof repair", "the page must list the caller's note")
}

func TestNotesFragmentOmitsThePage(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "fragment@example.com").Token

	resp, raw := getHX(t, ts, token)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, string(raw), `id="notes-list"`, "the fragment is the list block on its own")
	assert.NotContains(t, string(raw), "<html", "a fragment must not carry the page shell")
	assert.Contains(t, string(raw), "No notes yet", "an empty list explains itself")
}

func TestFormCreateRendersACardAndAgreesWithJSON(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "author@example.com").Token

	values := url.Values{"title": {"Garden fence"}, "body": {"Replace two panels"}}
	resp, raw := postForm(t, ts, token, values, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, string(raw), "note-card", "an htmx create answers with the new card")
	assert.Contains(t, string(raw), "Garden fence")

	// The JSON API agrees: the form wrote through the same store, index and
	// owner as a JSON create.
	resp, raw = get(t, ts, "/api/notes/search?q=fence", token)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Garden fence", "a form-created note must be searchable")

	resp, raw = get(t, ts, "/api/notes", token)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "Garden fence", "a form-created note must be listed")
}

func TestFormCreateWithoutHtmxRedirectsToTheList(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "plain@example.com").Token

	values := url.Values{"title": {"Plain submit"}, "body": {"no javascript"}}
	resp, _ := postForm(t, ts, token, values, false)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/notes", resp.Header.Get("Location"))

	resp, raw := get(t, ts, "/notes", token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(raw), "Plain submit", "the redirect must land on a list holding the note")
}

func TestFormCreateRejectsAnEmptyTitleWith422AndFlash(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "particular@example.com").Token

	// Over htmx the answer is the form block again, carrying the flash and the
	// body the caller already typed.
	values := url.Values{"title": {"   "}, "body": {"the body survives"}}
	resp, raw := postForm(t, ts, token, values, true)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "A title is required.", "a validation error must flash")
	assert.Contains(t, string(raw), "field-error", "a validation error must mark the field, not only flash")
	assert.Contains(t, string(raw), "the body survives", "a validation error must keep what was typed")

	// Without htmx the answer is the whole page at the same status, so a plain
	// submit still explains itself.
	resp, raw = postForm(t, ts, token, values, false)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "<html", "a plain submit gets the page, not a fragment")
	assert.Contains(t, string(raw), "A title is required.")
}

// TestPaginationBlockNamesItsBranches renders the pagination block with and
// without a next page, so a truncated list says so instead of ending quietly.
func TestPaginationBlockNamesItsBranches(t *testing.T) {
	tmpl, err := ParseTemplates()
	require.NoError(t, err)

	var full bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&full, "pagination",
		notesPageData{Notes: []Note{sampleNote()}}))
	assert.Contains(t, full.String(), "Showing 1 note.", "the block names its count")

	var more bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&more, "pagination",
		notesPageData{Notes: []Note{sampleNote()}, HasMore: true}))
	assert.Contains(t, more.String(), "Showing 1 note.", "the truncated list still names its count")
	assert.Contains(t, more.String(), "Older notes are beyond this page.")
}

func TestNoteMarkupIsEscapedInTheUI(t *testing.T) {
	ts := newTestServer(t)
	token := signup(t, ts, "markup@example.com").Token

	resp, raw := postNote(t, ts, token, `{"title":"<script>alert(1)</script>","body":"<b>bold</b>"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	for _, fetch := range []func() (*http.Response, []byte){
		func() (*http.Response, []byte) { return get(t, ts, "/notes", token) },
		func() (*http.Response, []byte) { return getHX(t, ts, token) },
	} {
		resp, raw := fetch()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(raw), "&lt;script&gt;")
		assert.NotContains(t, string(raw), "<script>alert(1)")
		assert.NotContains(t, string(raw), "<b>bold</b>")
	}
}

func TestNotesUIIsScopedToOwner(t *testing.T) {
	ts := newTestServer(t)
	tokenA := signup(t, ts, "alice-ui@example.com").Token
	tokenB := signup(t, ts, "bob-ui@example.com").Token

	values := url.Values{"title": {"Alice roof"}, "body": {"Slate tiles"}}
	resp, raw := postForm(t, ts, tokenA, values, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	id := cardID(t, string(raw))

	// Another account's page, fragment and search-equivalent list show nothing.
	resp, raw = get(t, ts, "/notes", tokenB)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, string(raw), "Alice roof")

	resp, raw = getHX(t, ts, tokenB)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, string(raw), "Alice roof")
	assert.Contains(t, string(raw), "No notes yet")

	// And their delete of it is the same 404 as for a row that does not exist.
	assert.Equal(t, http.StatusNotFound, deleteNote(t, ts, "/notes/"+id, tokenB, true).StatusCode)

	// The owner's copy survived all of that, and their own delete works: over
	// htmx the card answers 200 with nothing to swap in, without it a redirect
	// back to the list.
	resp, raw = getHX(t, ts, tokenA)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(raw), "Alice roof")

	assert.Equal(t, http.StatusOK, deleteNote(t, ts, "/notes/"+id, tokenA, true).StatusCode)

	resp, raw = getHX(t, ts, tokenA)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, string(raw), "Alice roof")

	values = url.Values{"title": {"Second"}, "body": {""}}
	resp, raw = postForm(t, ts, tokenA, values, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	second := cardID(t, string(raw))
	resp2 := deleteNote(t, ts, "/notes/"+second, tokenA, false)
	assert.Equal(t, http.StatusSeeOther, resp2.StatusCode)
	assert.Equal(t, "/notes", resp2.Header.Get("Location"))
}

// cardID reads the new card's note id out of the fragment that rendered it.
func cardID(t *testing.T, fragment string) string {
	t.Helper()
	const prefix = `id="note-`
	i := strings.Index(fragment, prefix)
	require.NotEqual(t, -1, i, "the card fragment must carry the note id: %s", fragment)
	rest := fragment[i+len(prefix):]
	j := strings.IndexByte(rest, '"')
	require.NotEqual(t, -1, j, "the card fragment must close the note id: %s", fragment)
	return rest[:j]
}

func TestStaticAssetsServeWithTheirCachePolicy(t *testing.T) {
	ts := newTestServer(t)

	resp, raw := get(t, ts, "/static/css/tokens.css", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(raw), "[data-theme=dashboard]", "the tokens must scope the dashboard theme")
	assert.Contains(t, string(raw), "[data-theme=landing]", "the tokens must scope the landing theme")
	assert.Contains(t, resp.Header.Get("Cache-Control"), "no-cache", "author styles must revalidate")

	resp, _ = get(t, ts, "/static/vendor/htmx.min.js", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Cache-Control"), "immutable", "vendored scripts are content-addressed by VERSION")

	resp, raw = get(t, ts, "/static/favicon.svg", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "svg")
}

// The landing shell owns GET /, so there is no root test here: the UI boot
// path this piece proves is GET /notes above.
