package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/httpapi"
	"github.com/ManavA/keel/httpx"
)

// requireError checks an error response: the status, and the generic body
// httpx writes, which says nothing of what went wrong.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	require.Equal(t, status, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	body := decodeBody[httpx.ErrorBody](t, rec)
	assert.Equal(t, strings.ToLower(http.StatusText(status)), body.Error)
}

// A store's own words never reach a client: the body is the generic one.
var errStoreDown = errors.New("pq: relation agent_runs does not exist")

func requireNoLeak(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.NotContains(t, rec.Body.String(), "agent_runs")
	assert.NotContains(t, rec.Body.String(), "pq:")
}

func TestNew(t *testing.T) {
	t.Run("refuses nil Runs", func(t *testing.T) {
		api, err := httpapi.New(httpapi.Options{})
		require.Error(t, err)
		assert.Nil(t, api)
	})
	t.Run("Runs alone is enough", func(t *testing.T) {
		api, err := httpapi.New(httpapi.Options{Runs: newFake()})
		require.NoError(t, err)
		require.NotNil(t, api)
		assert.NotNil(t, api.Routes())
	})
}

func TestRoutes_AreTheDocumentedOnes(t *testing.T) {
	r := newAPI(t, newFake())

	var got []string
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}))
	slices.Sort(got)

	want := []string{
		"GET /approvals",
		"GET /runs",
		"GET /runs/{id}",
		"GET /runs/{id}/events",
		"GET /runs/{id}/timeline",
		"HEAD /runs/{id}/events",
		"POST /approvals/{id}/approve",
		"POST /approvals/{id}/decline",
		"POST /runs/{id}/cancel",
	}
	assert.Equal(t, want, got, "the surface has no route that starts a run")
}

func TestGetRun(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(4)))
	h := newAPI(t, f)

	t.Run("serves the run", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/runs/"+uid(1), "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
		want, err := json.Marshal(f.runs[uid(1)])
		require.NoError(t, err)
		assert.JSONEq(t, string(want), rec.Body.String())
	})
}

// Every route that names a run or an approval answers a name the store does
// not know with 404, and gives the store the name exactly as it was written.
func TestReadRoutes_Failures(t *testing.T) {
	routes := []struct{ name, path string }{
		{"run", "/runs/%s"},
		{"timeline", "/runs/%s/timeline"},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			t.Run("unknown id", func(t *testing.T) {
				f := newFake()
				rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(route.path, uid(9)), "")
				requireError(t, rec, http.StatusNotFound)
			})
			t.Run("a wrapped not-found is still 404", func(t *testing.T) {
				f := newFake().fail("GetRun", fmt.Errorf("get: %w", agent.ErrNotFound)).
					fail("Changes", fmt.Errorf("changes: %w", agent.ErrNotFound))
				rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(route.path, uid(1)), "")
				requireError(t, rec, http.StatusNotFound)
			})
			t.Run("store error", func(t *testing.T) {
				f := newFake().addRun(mkRun(uid(1))).fail("GetRun", errStoreDown).fail("Changes", errStoreDown)
				rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(route.path, uid(1)), "")
				requireError(t, rec, http.StatusInternalServerError)
				requireNoLeak(t, rec)
			})
			// The form of an id is the store's to judge: a name that is not a
			// canonical UUID reaches it as written and comes back not found.
			for _, id := range []string{
				"abc",
				strings.ToUpper(letterID),
				"{" + uid(1) + "}",
				"00000000000040008000000000000001",
				"%20",
			} {
				t.Run("id "+id, func(t *testing.T) {
					f := newFake().addRun(mkRun(uid(1))).addRun(mkRun(letterID))
					rec := do(t, newAPI(t, f), http.MethodGet, fmt.Sprintf(route.path, url.PathEscape(id)), "")
					requireError(t, rec, http.StatusNotFound)
					f.mu.Lock()
					defer f.mu.Unlock()
					want, err := url.PathUnescape(url.PathEscape(id))
					require.NoError(t, err)
					if route.name == "run" {
						assert.Equal(t, []string{want}, f.getIDs)
					} else {
						require.Len(t, f.changes, 1)
						assert.Equal(t, want, f.changes[0].RunID)
					}
				})
			}
		})
	}
}

func TestTimeline(t *testing.T) {
	approval := mkApproval(uid(50), uid(1), 2, 3)

	t.Run("serves the run, its steps and its approvals from the start", func(t *testing.T) {
		f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting), withRev(3)))
		f.edit(func(f *fakeRuns) {
			f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2), mkStep(uid(1), 2, 3)}
			f.approvals = []agent.Approval{approval}
		})
		rec := do(t, newAPI(t, f), http.MethodGet, "/runs/"+uid(1)+"/timeline", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

		got := decodeBody[struct {
			Run       agent.Run        `json:"run"`
			Steps     []agent.Step     `json:"steps"`
			Approvals []agent.Approval `json:"approvals"`
		}](t, rec)
		assert.Equal(t, uid(1), got.Run.ID)
		require.Len(t, got.Steps, 2)
		assert.Equal(t, []int{1, 2}, []int{got.Steps[0].Seq, got.Steps[1].Seq}, "journal order, as the store gives it")
		require.Len(t, got.Approvals, 1)
		assert.Equal(t, uid(50), got.Approvals[0].ID)

		calls := f.changesCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, int64(0), calls[0].Since, "the timeline is everything since the start")
	})

	t.Run("a run with no steps or approvals has empty lists", func(t *testing.T) {
		f := newFake().addRun(mkRun(uid(1)))
		rec := do(t, newAPI(t, f), http.MethodGet, "/runs/"+uid(1)+"/timeline", "")
		require.Equal(t, http.StatusOK, rec.Code)
		raw := decodeBody[map[string]json.RawMessage](t, rec)
		assert.JSONEq(t, `[]`, string(raw["steps"]))
		assert.JSONEq(t, `[]`, string(raw["approvals"]))
	})
}

// A step's opaque form is the provider's private copy of a turn and can hold
// reasoning the service keeps to itself: no route serves it.
func TestTimeline_NeverServesOpaque(t *testing.T) {
	f := newFake().addRun(mkRun(uid(1), withRev(2)))
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
	rec := do(t, newAPI(t, f), http.MethodGet, "/runs/"+uid(1)+"/timeline", "")
	require.Equal(t, http.StatusOK, rec.Code)

	assert.NotContains(t, rec.Body.String(), secretThought)
	assert.NotContains(t, rec.Body.String(), "opaque")
	steps := decodeBody[struct {
		Steps []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"steps"`
	}](t, rec).Steps
	require.Len(t, steps, 1)
	assert.Contains(t, steps[0].Message, "text", "the rest of the message is served")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotNil(t, f.steps[uid(1)][0].Message.Opaque, "what the store returned is not changed")
}

func TestListRuns(t *testing.T) {
	t.Run("an empty list is [] and carries no cursor", func(t *testing.T) {
		rec := do(t, newAPI(t, newFake()), http.MethodGet, "/runs", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"runs": []}`, rec.Body.String())
	})

	t.Run("serves the runs newest first", func(t *testing.T) {
		f := newFake().
			addRun(mkRun(uid(1), withCreatedAt(at(1)))).
			addRun(mkRun(uid(2), withCreatedAt(at(3)))).
			addRun(mkRun(uid(3), withCreatedAt(at(2))))
		rec := do(t, newAPI(t, f), http.MethodGet, "/runs", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

		got := decodeBody[struct {
			Runs []agent.Run `json:"runs"`
			Next *string     `json:"next"`
		}](t, rec)
		var ids []string
		for _, r := range got.Runs {
			ids = append(ids, r.ID)
		}
		assert.Equal(t, []string{uid(2), uid(3), uid(1)}, ids)
		assert.Nil(t, got.Next, "a page that is not full has no cursor")
	})

	t.Run("store error", func(t *testing.T) {
		f := newFake().fail("ListRuns", errStoreDown)
		rec := do(t, newAPI(t, f), http.MethodGet, "/runs", "")
		requireError(t, rec, http.StatusInternalServerError)
		requireNoLeak(t, rec)
	})
}

func TestListRuns_Filters(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  agent.RunFilter
	}{
		{"none", "", agent.RunFilter{Limit: 50}},
		{"status", "?status=waiting", agent.RunFilter{Status: agent.StatusWaiting, Limit: 50}},
		{"agent", "?agent=reviewer", agent.RunFilter{Agent: "reviewer", Limit: 50}},
		{"parent", "?parent=" + uid(7), agent.RunFilter{ParentID: uid(7), Limit: 50}},
		{"limit", "?limit=7", agent.RunFilter{Limit: 7}},
		{"all of them", "?status=failed&agent=coordinator&parent=" + uid(7) + "&limit=200",
			agent.RunFilter{Status: agent.StatusFailed, Agent: "coordinator", ParentID: uid(7), Limit: 200}},
		{"empty values are no filter", "?status=&agent=&parent=&cursor=", agent.RunFilter{Limit: 50}},
		{"a parent that is not a UUID reaches the store", "?parent=abc", agent.RunFilter{ParentID: "abc", Limit: 50}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs"+tt.query, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, f.runFilters, 1)
			assert.Equal(t, tt.want, f.runFilters[0])
		})
	}
}

func TestListRuns_Statuses(t *testing.T) {
	for _, status := range []agent.Status{
		agent.StatusRunnable, agent.StatusWaiting, agent.StatusCompleted, agent.StatusFailed, agent.StatusCancelled,
	} {
		t.Run("accepts "+string(status), func(t *testing.T) {
			f := newFake().
				addRun(mkRun(uid(1), withStatus(status))).
				addRun(mkRun(uid(2), withStatus(agent.StatusRunnable), withCreatedAt(at(1))))
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs?status="+string(status), "")
			require.Equal(t, http.StatusOK, rec.Code)
			got := decodeBody[struct{ Runs []agent.Run }](t, rec)
			require.NotEmpty(t, got.Runs)
			for _, r := range got.Runs {
				assert.Equal(t, status, r.Status)
			}
		})
	}
	for _, status := range []string{"bogus", "WAITING", "done", "Waiting", "waiting%20"} {
		t.Run("refuses "+status, func(t *testing.T) {
			f := newFake()
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs?status="+status, "")
			requireError(t, rec, http.StatusBadRequest)
			assert.Empty(t, f.calls, "an unknown status is refused before the store is asked")
		})
	}
}

// A limit that cannot be honoured is refused and never clamped: a client that
// asked for a million and got 200 would take the page for the end of the list.
func TestListRuns_Limit(t *testing.T) {
	for _, tt := range []struct {
		query string
		want  int
	}{
		{"", 50},
		{"?limit=1", 1},
		{"?limit=50", 50},
		{"?limit=200", 200},
	} {
		t.Run("accepts "+tt.query, func(t *testing.T) {
			f := newFake()
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs"+tt.query, "")
			require.Equal(t, http.StatusOK, rec.Code)
			require.Len(t, f.runFilters, 1)
			assert.Equal(t, tt.want, f.runFilters[0].Limit)
		})
	}
	for _, query := range []string{
		"?limit=0", "?limit=201", "?limit=abc", "?limit=-1", "?limit=1.5", "?limit=",
		"?limit=%205", "?limit=9999999999999999999", "?limit=1&limit=2",
	} {
		t.Run("refuses "+query, func(t *testing.T) {
			f := newFake()
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs"+query, "")
			requireError(t, rec, http.StatusBadRequest)
			assert.Empty(t, f.calls)
		})
	}
	t.Run("refuses a filter given twice", func(t *testing.T) {
		for _, query := range []string{"?status=waiting&status=failed", "?agent=a&agent=b", "?parent=a&parent=b"} {
			rec := do(t, newAPI(t, newFake()), http.MethodGet, "/runs"+query, "")
			requireError(t, rec, http.StatusBadRequest)
		}
	})
}

// walk follows next from one page to the following until there is none, and
// returns the ids of each page.
func walk(t *testing.T, h http.Handler, firstQuery string) [][]string {
	t.Helper()
	var pages [][]string
	query := firstQuery
	for range 20 {
		rec := do(t, h, http.MethodGet, "/runs"+query, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decodeBody[struct {
			Runs []agent.Run `json:"runs"`
			Next string      `json:"next"`
		}](t, rec)
		page := []string{}
		for _, r := range got.Runs {
			page = append(page, r.ID)
		}
		pages = append(pages, page)
		if got.Next == "" {
			return pages
		}
		query = firstQuery + "&cursor=" + url.QueryEscape(got.Next)
	}
	t.Fatal("the cursor never ran out")
	return nil
}

func TestListRuns_CursorCarriesOnePageToTheNext(t *testing.T) {
	t.Run("every run once, newest first, with ties between runs made at the same moment", func(t *testing.T) {
		f := newFake()
		created := []int{1, 1, 1, 2, 2, 3, 4} // runs 1..7: three share a moment, two share another
		for i, sec := range created {
			f.addRun(mkRun(uid(i+1), withCreatedAt(at(sec))))
		}
		pages := walk(t, newAPI(t, f), "?limit=3")
		assert.Equal(t, [][]string{
			{uid(7), uid(6), uid(5)},
			{uid(4), uid(3), uid(2)},
			{uid(1)},
		}, pages)
	})

	t.Run("a full last page leads to one empty page", func(t *testing.T) {
		f := newFake()
		for i := 1; i <= 4; i++ {
			f.addRun(mkRun(uid(i), withCreatedAt(at(i))))
		}
		pages := walk(t, newAPI(t, f), "?limit=2")
		assert.Equal(t, [][]string{{uid(4), uid(3)}, {uid(2), uid(1)}, {}}, pages)
	})

	t.Run("the cursor is the position of the last run on the page", func(t *testing.T) {
		f := newFake()
		for i := 1; i <= 3; i++ {
			f.addRun(mkRun(uid(i), withCreatedAt(at(i))))
		}
		h := newAPI(t, f)
		first := decodeBody[struct{ Next string }](t, do(t, h, http.MethodGet, "/runs?limit=2", ""))
		require.NotEmpty(t, first.Next)
		rec := do(t, h, http.MethodGet, "/runs?limit=2&cursor="+url.QueryEscape(first.Next), "")
		require.Equal(t, http.StatusOK, rec.Code)

		require.Len(t, f.runFilters, 2)
		require.NotNil(t, f.runFilters[1].Before)
		assert.Equal(t, uid(2), f.runFilters[1].Before.ID)
		assert.True(t, f.runFilters[1].Before.CreatedAt.Equal(at(2)), "got %v", f.runFilters[1].Before.CreatedAt)
		assert.Nil(t, f.runFilters[0].Before)
	})

	t.Run("filters apply on every page", func(t *testing.T) {
		f := newFake()
		for i := 1; i <= 6; i++ {
			name := "a"
			if i%2 == 0 {
				name = "b"
			}
			f.addRun(mkRun(uid(i), withCreatedAt(at(i)), withAgent(name)))
		}
		pages := walk(t, newAPI(t, f), "?agent=a&limit=2")
		assert.Equal(t, [][]string{{uid(5), uid(3)}, {uid(1)}}, pages)
	})
}

func TestListRuns_RefusesACursorItCannotRead(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cursorOf := func(at, id string) string { return "?cursor=" + enc(`{"t":`+at+`,"id":`+id+`}`) }
	const when = `"2026-01-02T03:04:05Z"`
	quoted := strconv.Quote
	for _, tt := range []struct{ name, query string }{
		{"not base64", "?cursor=!!!"},
		{"not JSON", "?cursor=" + enc("not json")},
		{"an empty object", "?cursor=" + enc(`{}`)},
		{"no id", "?cursor=" + enc(`{"t":`+when+`}`)},
		{"no time", "?cursor=" + enc(`{"id":`+quoted(uid(1))+`}`)},
		{"an empty id", cursorOf(when, `""`)},
		{"a time that is not one", cursorOf(`"yesterday"`, quoted(uid(1)))},
		{"a time with no zone", cursorOf(`"2026-01-02T03:04:05"`, quoted(uid(1)))},
		{"a time that is a number", cursorOf(`1767323045`, quoted(uid(1)))},
		{"an id that is a number", cursorOf(when, `5`)},
		{"an id that is not a UUID", cursorOf(when, `"abc"`)},
		{"an id with a space after it", cursorOf(when, quoted(uid(1)+" "))},
		// Each of these is a UUID that uuid.Parse reads, in a spelling other
		// than the one ids are kept in.
		{"an id in upper case", cursorOf(when, quoted(strings.ToUpper(letterID)))},
		{"an id in braces", cursorOf(when, quoted("{"+letterID+"}"))},
		{"an id as a URN", cursorOf(when, quoted("urn:uuid:"+letterID))},
		{"an id with no hyphens", cursorOf(when, quoted(strings.ReplaceAll(letterID, "-", "")))},
		{"given twice", "?cursor=a&cursor=b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			rec := do(t, newAPI(t, f), http.MethodGet, "/runs"+tt.query, "")
			requireError(t, rec, http.StatusBadRequest)
			assert.Empty(t, f.calls, "a cursor that does not read is refused before the store is asked, and is never an empty first page")
		})
	}

	t.Run("an id in the canonical form is read", func(t *testing.T) {
		f := newFake()
		rec := do(t, newAPI(t, f), http.MethodGet, "/runs"+cursorOf(when, quoted(letterID)), "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, f.runFilters, 1)
		require.NotNil(t, f.runFilters[0].Before)
		assert.Equal(t, letterID, f.runFilters[0].Before.ID)
	})
}

func TestListApprovals(t *testing.T) {
	t.Run("serves the approvals oldest first", func(t *testing.T) {
		f := newFake()
		f.edit(func(f *fakeRuns) {
			f.approvals = []agent.Approval{
				mkApproval(uid(52), uid(1), 3, 5),
				mkApproval(uid(51), uid(1), 1, 3),
				mkApproval(uid(53), uid(2), 2, 4),
			}
		})
		rec := do(t, newAPI(t, f), http.MethodGet, "/approvals", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
		got := decodeBody[struct {
			Approvals []agent.Approval `json:"approvals"`
		}](t, rec)
		var ids []string
		for _, a := range got.Approvals {
			ids = append(ids, a.ID)
		}
		assert.Equal(t, []string{uid(51), uid(53), uid(52)}, ids)
	})

	t.Run("an empty list is []", func(t *testing.T) {
		rec := do(t, newAPI(t, newFake()), http.MethodGet, "/approvals", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"approvals": []}`, rec.Body.String())
	})

	t.Run("filters", func(t *testing.T) {
		for _, tt := range []struct {
			name, query string
			want        agent.ApprovalFilter
		}{
			{"none", "", agent.ApprovalFilter{Limit: 50}},
			{"status", "?status=pending", agent.ApprovalFilter{Status: agent.ApprovalPending, Limit: 50}},
			{"run", "?run=" + uid(3), agent.ApprovalFilter{RunID: uid(3), Limit: 50}},
			{"limit", "?limit=9", agent.ApprovalFilter{Limit: 9}},
			{"all of them", "?status=expired&run=" + uid(3) + "&limit=200",
				agent.ApprovalFilter{Status: agent.ApprovalExpired, RunID: uid(3), Limit: 200}},
			{"empty values are no filter", "?status=&run=", agent.ApprovalFilter{Limit: 50}},
			{"a run that is not a UUID reaches the store", "?run=abc", agent.ApprovalFilter{RunID: "abc", Limit: 50}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFake()
				rec := do(t, newAPI(t, f), http.MethodGet, "/approvals"+tt.query, "")
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Len(t, f.approvalFilters, 1)
				assert.Equal(t, tt.want, f.approvalFilters[0])
			})
		}
	})

	t.Run("each status is accepted", func(t *testing.T) {
		for _, status := range []agent.ApprovalStatus{
			agent.ApprovalPending, agent.ApprovalApproved, agent.ApprovalDeclined,
			agent.ApprovalExpired, agent.ApprovalCancelled,
		} {
			rec := do(t, newAPI(t, newFake()), http.MethodGet, "/approvals?status="+string(status), "")
			assert.Equal(t, http.StatusOK, rec.Code, status)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for _, query := range []string{
			"?status=bogus", "?status=Pending", "?status=waiting",
			"?limit=0", "?limit=201", "?limit=abc", "?limit=",
			"?limit=1&limit=2", "?status=pending&status=expired", "?run=a&run=b",
		} {
			t.Run(query, func(t *testing.T) {
				f := newFake()
				rec := do(t, newAPI(t, f), http.MethodGet, "/approvals"+query, "")
				requireError(t, rec, http.StatusBadRequest)
				assert.Empty(t, f.calls)
			})
		}
	})

	t.Run("store error", func(t *testing.T) {
		f := newFake().fail("ListApprovals", errStoreDown)
		rec := do(t, newAPI(t, f), http.MethodGet, "/approvals", "")
		requireError(t, rec, http.StatusInternalServerError)
		requireNoLeak(t, rec)
	})
}

// mutation is one of the three routes that change something.
type mutation struct {
	name    string
	path    string // with the id of what it changes
	status  int    // on success
	method  string // the Runs method it reaches
	pending string // the id of what exists
	// seen is what the engine was asked, if it was asked.
	seen func(f *fakeRuns) (call decisionCall, ok bool)
}

func mutations() []mutation {
	decisionSeen := func(f *fakeRuns) (decisionCall, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.decisions) == 0 {
			return decisionCall{}, false
		}
		return f.decisions[len(f.decisions)-1], true
	}
	return []mutation{
		{
			name: "cancel", path: "/runs/%s/cancel", status: http.StatusAccepted,
			method: "Cancel", pending: uid(1),
			seen: func(f *fakeRuns) (decisionCall, bool) {
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.cancels) == 0 {
					return decisionCall{}, false
				}
				c := f.cancels[len(f.cancels)-1]
				return decisionCall{Method: "Cancel", ID: c.RunID, By: c.By, Reason: c.Reason}, true
			},
		},
		{
			name: "approve", path: "/approvals/%s/approve", status: http.StatusOK,
			method: "Approve", pending: uid(50), seen: decisionSeen,
		},
		{
			name: "decline", path: "/approvals/%s/decline", status: http.StatusOK,
			method: "Decline", pending: uid(50), seen: decisionSeen,
		},
	}
}

// mutationFake holds a run and an approval for the routes that change them.
func mutationFake() *fakeRuns {
	f := newFake().addRun(mkRun(uid(1), withStatus(agent.StatusWaiting)))
	f.edit(func(f *fakeRuns) { f.approvals = []agent.Approval{mkApproval(uid(50), uid(1), 2, 3)} })
	return f
}

func TestMutations(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)

			t.Run("succeeds, with the actor's name and the reason reaching the engine", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, `{"reason":"looks fine"}`, "X-Actor", "sam")
				require.Equal(t, m.status, rec.Code, rec.Body.String())
				call, ok := m.seen(f)
				require.True(t, ok)
				assert.Equal(t, decisionCall{Method: m.method, ID: m.pending, By: "sam", Reason: "looks fine"}, call)
			})

			t.Run("a request with no body is accepted", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, "", "X-Actor", "sam")
				require.Equal(t, m.status, rec.Code, rec.Body.String())
				call, ok := m.seen(f)
				require.True(t, ok)
				assert.Equal(t, "", call.Reason)
			})

			t.Run("so are an empty object and whitespace", func(t *testing.T) {
				for _, body := range []string{`{}`, "  \n", `{"reason":""}`, `{"reason":null}`} {
					f := mutationFake()
					rec := do(t, newAPI(t, f), http.MethodPost, path, body, "X-Actor", "sam")
					assert.Equal(t, m.status, rec.Code, "body %q: %s", body, rec.Body.String())
				}
			})

			t.Run("403 without an Actor", func(t *testing.T) {
				f := mutationFake()
				h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = nil })
				rec := do(t, h, http.MethodPost, path, `{"reason":"x"}`, "X-Actor", "sam")
				requireError(t, rec, http.StatusForbidden)
				assert.Empty(t, f.calls, "a refused request reaches nothing")
			})

			t.Run("403 when the Actor names nobody", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, `{"reason":"x"}`) // no X-Actor header
				requireError(t, rec, http.StatusForbidden)
				assert.Empty(t, f.calls)
			})

			t.Run("403 comes before the body is looked at", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, `{"bogus":`)
				requireError(t, rec, http.StatusForbidden)
			})

			t.Run("the Actor sees the request", func(t *testing.T) {
				f := mutationFake()
				var seen *http.Request
				h := newAPI(t, f, func(o *httpapi.Options) {
					o.Actor = func(r *http.Request) string { seen = r; return "from the request" }
				})
				rec := do(t, h, http.MethodPost, path, "")
				require.Equal(t, m.status, rec.Code)
				require.NotNil(t, seen)
				assert.Equal(t, "POST", seen.Method)
				call, _ := m.seen(f)
				assert.Equal(t, "from the request", call.By)
			})

			t.Run("a body is read to 4 KiB and no more", func(t *testing.T) {
				const overhead = len(`{"reason":""}`)
				exact := `{"reason":"` + strings.Repeat("a", 4096-overhead) + `"}`
				require.Len(t, exact, 4096)

				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, exact, "X-Actor", "sam")
				assert.Equal(t, m.status, rec.Code, "exactly 4 KiB is allowed")

				f = mutationFake()
				over := `{"reason":"` + strings.Repeat("a", 4096-overhead+1) + `"}`
				rec = do(t, newAPI(t, f), http.MethodPost, path, over, "X-Actor", "sam")
				requireError(t, rec, http.StatusBadRequest)
				assert.Empty(t, f.calls)
			})

			t.Run("a body that is mostly padding is over the limit too", func(t *testing.T) {
				f := mutationFake()
				body := `{}` + strings.Repeat(" ", 5000)
				rec := do(t, newAPI(t, f), http.MethodPost, path, body, "X-Actor", "sam")
				requireError(t, rec, http.StatusBadRequest)
				assert.Empty(t, f.calls)
			})

			t.Run("400 for a body that is not what is expected", func(t *testing.T) {
				for _, body := range []string{
					`{"reason":"x","extra":1}`,        // an unknown field
					`{"Reason":"x","by":"someone"}`,   // one of them is unknown
					`{"reason":`,                      // cut short
					`not json`,                        // not JSON
					`{"reason":5}`,                    // the wrong type
					`["reason"]`,                      // not an object
					`"reason"`,                        // not an object
					`{"reason":"a"} {"reason":"b"}`,   // two values
					`{"reason":"a"} trailing garbage`, // something after it
				} {
					f := mutationFake()
					rec := do(t, newAPI(t, f), http.MethodPost, path, body, "X-Actor", "sam")
					requireError(t, rec, http.StatusBadRequest)
					assert.Empty(t, f.calls, "body %q", body)
				}
			})

			t.Run("409 when the engine says it is settled", func(t *testing.T) {
				for _, err := range []error{
					agent.ErrAlreadyDecided, agent.ErrFinished,
					fmt.Errorf("decide: %w", agent.ErrAlreadyDecided), fmt.Errorf("cancel: %w", agent.ErrFinished),
				} {
					f := mutationFake().fail(m.method, err)
					rec := do(t, newAPI(t, f), http.MethodPost, path, "", "X-Actor", "sam")
					requireError(t, rec, http.StatusConflict)
				}
			})

			t.Run("404 for an unknown id", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, fmt.Sprintf(m.path, uid(99)), "", "X-Actor", "sam")
				requireError(t, rec, http.StatusNotFound)
			})

			t.Run("404 for an id that is not a UUID, which the engine is given as it came", func(t *testing.T) {
				f := mutationFake().fail(m.method, agent.ErrNotFound)
				rec := do(t, newAPI(t, f), http.MethodPost, fmt.Sprintf(m.path, "abc"), "", "X-Actor", "sam")
				requireError(t, rec, http.StatusNotFound)
				call, ok := m.seen(f)
				require.True(t, ok)
				assert.Equal(t, "abc", call.ID)
			})

			t.Run("store error", func(t *testing.T) {
				f := mutationFake().fail(m.method, errStoreDown)
				rec := do(t, newAPI(t, f), http.MethodPost, path, "", "X-Actor", "sam")
				requireError(t, rec, http.StatusInternalServerError)
				requireNoLeak(t, rec)
			})

			t.Run("only POST changes anything", func(t *testing.T) {
				for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
					f := mutationFake()
					rec := do(t, newAPI(t, f), method, path, "", "X-Actor", "sam")
					assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
					assert.Empty(t, f.calls)
				}
			})
		})
	}
}

// The three routes that decide things refuse a browser request that comes from
// another origin, whatever it carries, before the actor or the engine is
// asked: a page on another site cannot approve anything by making a browser
// post a form. A request with no Sec-Fetch-Site and no Origin is not a browser
// speaking for a page, and is passed to the actor like any other.
func TestMutations_RefuseACrossOriginBrowserRequest(t *testing.T) {
	trusting := func(origin string) *http.CrossOriginProtection {
		p := http.NewCrossOriginProtection()
		require.NoError(t, p.AddTrustedOrigin(origin))
		return p
	}
	const front = "https://front.example"
	tests := []struct {
		name       string
		headers    []string
		protection *http.CrossOriginProtection // nil is the default
		allowed    bool
	}{
		{"cross-site", []string{"Sec-Fetch-Site", "cross-site"}, nil, false},
		{"same-site, which is another origin", []string{"Sec-Fetch-Site", "same-site"}, nil, false},
		{"same-origin", []string{"Sec-Fetch-Site", "same-origin"}, nil, true},
		{"none, a request the user made", []string{"Sec-Fetch-Site", "none"}, nil, true},
		{"neither header", nil, nil, true},
		{"an Origin that differs from the host", []string{"Origin", "https://other.example"}, nil, false},
		{"an Origin that is the host", []string{"Origin", "http://example.com"}, nil, true},
		// What a sandboxed frame, a page from a file and some redirects send.
		{"an Origin that is null", []string{"Origin", "null"}, nil, false},
		{"an Origin that is null, with an origin trusted", []string{"Origin", "null"}, trusting(front), false},
		{"cross-site with an Origin that is null", []string{"Sec-Fetch-Site", "cross-site", "Origin", "null"}, nil, false},
		{"cross-site from an origin that is trusted", []string{"Sec-Fetch-Site", "cross-site", "Origin", front}, trusting(front), true},
		{"an Origin that is trusted", []string{"Origin", front}, trusting(front), true},
		{"the same, when none is trusted", []string{"Sec-Fetch-Site", "cross-site", "Origin", front}, nil, false},
		{"cross-site from another origin than the trusted one", []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://other.example"}, trusting(front), false},
	}
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					f := mutationFake()
					var asked int
					h := newAPI(t, f, func(o *httpapi.Options) {
						o.CrossOrigin = tt.protection
						o.Actor = func(r *http.Request) string { asked++; return "sam" }
					})
					rec := do(t, h, http.MethodPost, path, `{"reason":"x"}`, tt.headers...)
					if tt.allowed {
						require.Equal(t, m.status, rec.Code, rec.Body.String())
						call, ok := m.seen(f)
						require.True(t, ok)
						assert.Equal(t, "sam", call.By)
						return
					}
					requireError(t, rec, http.StatusForbidden)
					assert.Empty(t, f.calls, "a refused request reaches nothing")
					assert.Zero(t, asked, "and the actor is not asked about it")
				})
			}
		})
	}
}

// What the guard is for is changing something. Reading is not, and a page on
// another site is not made safe by being refused a read the user could make.
func TestReadRoutes_AreNotGuardedAgainstCrossOrigin(t *testing.T) {
	f := endedRun(agent.StatusCompleted)
	h := newAPI(t, f, quick)
	for _, path := range []string{
		"/runs", "/runs/" + uid(1), "/runs/" + uid(1) + "/timeline", "/approvals", "/runs/" + uid(1) + "/events",
	} {
		for _, headers := range [][]string{
			{"Sec-Fetch-Site", "cross-site"},
			{"Origin", "https://other.example"},
			{"Sec-Fetch-Site", "cross-site", "Origin", "https://other.example"},
		} {
			rec := do(t, h, http.MethodGet, path, "", headers...)
			assert.Equal(t, http.StatusOK, rec.Code, "%s with %v", path, headers)
		}
	}
}

// A name of nothing but white space is nobody: a decision recorded under it
// would be a decision by no one.
func TestMutations_RefuseAnActorThatNamesNobody(t *testing.T) {
	names := map[string]string{
		"empty":                    "",
		"a space":                  " ",
		"a tab":                    "\t",
		"a line feed":              "\n",
		"a no-break space":         "\u00a0",
		"an em space":              "\u2003",
		"a next line":              "\u0085",
		"several kinds of spacing": " \t\r\n\u00a0\u2003 ",
	}
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)
			for name, who := range names {
				t.Run(name, func(t *testing.T) {
					f := mutationFake()
					h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = func(*http.Request) string { return who } })
					rec := do(t, h, http.MethodPost, path, `{"reason":"x"}`)
					requireError(t, rec, http.StatusForbidden)
					assert.Empty(t, f.calls, "a decision by nobody reaches nothing")
				})
			}
			t.Run("the spacing round a name is not part of it", func(t *testing.T) {
				f := mutationFake()
				h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = func(*http.Request) string { return " \tsam lee\u00a0\n" } })
				rec := do(t, h, http.MethodPost, path, "")
				require.Equal(t, m.status, rec.Code, rec.Body.String())
				call, ok := m.seen(f)
				require.True(t, ok)
				assert.Equal(t, "sam lee", call.By)
			})
		})
	}
}

// A reason is text a database keeps: it has no NUL and is valid UTF-8, and the
// body that carries it is already bounded at 4 KiB.
func TestMutations_RefuseAReasonADatabaseCouldNotKeep(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)
			for name, body := range map[string]string{
				"a NUL escaped":                 `{"reason":"before\u0000after"}`,
				"a NUL at the end":              `{"reason":"x\u0000"}`,
				"a byte that is not UTF-8":      "{\"reason\":\"caf\xe9\"}",
				"the start of a sequence":       "{\"reason\":\"\xe2\x82\"}",
				"an overlong encoding":          "{\"reason\":\"\xc0\xaf\"}",
				"a surrogate encoded as UTF-8":  "{\"reason\":\"\xed\xa0\x80\"}",
				"bytes that are not UTF-8 only": "\xff\xfe",
			} {
				t.Run(name, func(t *testing.T) {
					f := mutationFake()
					rec := do(t, newAPI(t, f), http.MethodPost, path, body, "X-Actor", "sam")
					requireError(t, rec, http.StatusBadRequest)
					assert.Empty(t, f.calls)
				})
			}
			t.Run("text in any script is kept as it is", func(t *testing.T) {
				for _, reason := range []string{"caf\u00e9", "\u65e5\u672c\u8a9e", "emoji \U0001F600", "tab\there", "replacement \ufffd", "line\nbreak"} {
					f := mutationFake()
					body, err := json.Marshal(map[string]string{"reason": reason})
					require.NoError(t, err)
					rec := do(t, newAPI(t, f), http.MethodPost, path, string(body), "X-Actor", "sam")
					require.Equal(t, m.status, rec.Code, rec.Body.String())
					call, ok := m.seen(f)
					require.True(t, ok)
					assert.Equal(t, reason, call.Reason)
				}
			})
		})
	}
}

// A client that has gone, or a request whose time ran out, is not a fault of
// the store whatever the store was doing when it found out.
func TestFailures_WhenTheRequestIsOver(t *testing.T) {
	type route struct {
		name, method, path, body, call string
		stream                         bool
	}
	routes := []route{
		{"run", http.MethodGet, "/runs/" + uid(1), "", "GetRun", false},
		{"timeline", http.MethodGet, "/runs/" + uid(1) + "/timeline", "", "Changes", false},
		{"list of runs", http.MethodGet, "/runs", "", "ListRuns", false},
		{"list of approvals", http.MethodGet, "/approvals", "", "ListApprovals", false},
		{"cancel", http.MethodPost, "/runs/" + uid(1) + "/cancel", "", "Cancel", false},
		{"approve", http.MethodPost, "/approvals/" + uid(50) + "/approve", "", "Approve", false},
		{"decline", http.MethodPost, "/approvals/" + uid(50) + "/decline", "", "Decline", false},
		{"the first read of a stream", http.MethodGet, "/runs/" + uid(1) + "/events", "", "Changes", true},
		{"head of a stream", http.MethodHead, "/runs/" + uid(1) + "/events", "", "GetRun", true},
	}
	gone := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	timedOut := func() context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			serve := func(t *testing.T, ctx context.Context, err error) (*httptest.ResponseRecorder, *logSink) {
				var sink logSink
				f := mutationFake().fail(r.call, err)
				h := newAPI(t, f, func(o *httpapi.Options) { o.Logger = sink.logger() })
				return doCtx(t, ctx, h, r.method, r.path, r.body, "X-Actor", "sam"), &sink
			}

			// A cancelled context does not say the client has gone: a server
			// shutting down, a client that half-closes after sending and a mount's
			// middleware all cancel it with the client still reading. The answer is
			// 503, which is true if the client is there and costs nothing if not,
			// and it is not an alarm.
			t.Run("a request that was cancelled, and failed, is 503 and no alarm", func(t *testing.T) {
				for name, err := range map[string]error{
					"a cancelled call":        fmt.Errorf("store: %w", context.Canceled),
					"any error at all":        errStoreDown,
					"a deadline of its own":   fmt.Errorf("store: %w", context.DeadlineExceeded),
					"the store's own failure": errors.New("connection reset"),
				} {
					rec, sink := serve(t, gone(), err)
					requireError(t, rec, http.StatusServiceUnavailable)
					assert.Empty(t, sink.atLeast(slog.LevelInfo), name)
					logged := sink.all()
					require.Len(t, logged, 1, name)
					assert.Equal(t, slog.LevelDebug, logged[0].Level, name)
					assert.Contains(t, logged[0].Attrs["error"], err.Error(), name)
				}
			})

			// What the engine answered is true whatever became of the request.
			t.Run("an answer the engine gave stands, in any state of the request", func(t *testing.T) {
				for _, ctx := range []context.Context{context.Background(), gone(), timedOut()} {
					for _, answer := range []struct {
						err  error
						want int
					}{
						{agent.ErrNotFound, http.StatusNotFound},
						{agent.ErrAlreadyDecided, http.StatusConflict},
						{agent.ErrFinished, http.StatusConflict},
						{fmt.Errorf("wrapped: %w", agent.ErrNotFound), http.StatusNotFound},
					} {
						rec, _ := serve(t, ctx, answer.err)
						requireError(t, rec, answer.want)
					}
				}
			})

			t.Run("a request whose time ran out, with the client still there, is 503", func(t *testing.T) {
				rec, _ := serve(t, timedOut(), fmt.Errorf("store: %w", context.DeadlineExceeded))
				requireError(t, rec, http.StatusServiceUnavailable)
			})

			t.Run("a store that gives up on its own, with the request live, is a fault", func(t *testing.T) {
				for _, err := range []error{
					fmt.Errorf("store: %w", context.Canceled),
					fmt.Errorf("store: %w", context.DeadlineExceeded),
				} {
					rec, sink := serve(t, context.Background(), err)
					requireError(t, rec, http.StatusInternalServerError)
					assert.NotEmpty(t, sink.atLeast(slog.LevelError))
				}
			})
		})
	}

}

// No route leaves the status it starts with after a failure. A handler that
// writes nothing answers 200, and a client that reads only the status takes a
// decision that was not made for one that was. Every route, with the request
// live, cancelled and past its deadline, and every kind of failure, answers
// with an error status and a body.
func TestFailures_NeverEndInASuccessStatus(t *testing.T) {
	type route struct{ pattern, path, call string }
	routes := []route{
		{"GET /runs", "/runs", "ListRuns"},
		{"GET /runs/{id}", "/runs/" + uid(1), "GetRun"},
		{"GET /runs/{id}/timeline", "/runs/" + uid(1) + "/timeline", "Changes"},
		{"GET /runs/{id}/events", "/runs/" + uid(1) + "/events", "Changes"},
		{"HEAD /runs/{id}/events", "/runs/" + uid(1) + "/events", "GetRun"},
		{"POST /runs/{id}/cancel", "/runs/" + uid(1) + "/cancel", "Cancel"},
		{"GET /approvals", "/approvals", "ListApprovals"},
		{"POST /approvals/{id}/approve", "/approvals/" + uid(50) + "/approve", "Approve"},
		{"POST /approvals/{id}/decline", "/approvals/" + uid(50) + "/decline", "Decline"},
	}

	t.Run("the table has every route", func(t *testing.T) {
		var served, tabled []string
		require.NoError(t, chi.Walk(newAPI(t, newFake()), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			served = append(served, method+" "+route)
			return nil
		}))
		for _, r := range routes {
			tabled = append(tabled, r.pattern)
		}
		slices.Sort(served)
		slices.Sort(tabled)
		assert.Equal(t, served, tabled, "a route added to the API is added to this table")
	})

	contexts := map[string]func(t *testing.T) context.Context{
		"live": func(*testing.T) context.Context { return context.Background() },
		"cancelled": func(*testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		},
		"past its deadline": func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		},
	}
	failures := map[string]error{
		"a store error":      errStoreDown,
		"a cancelled call":   fmt.Errorf("store: %w", context.Canceled),
		"a call out of time": fmt.Errorf("store: %w", context.DeadlineExceeded),
		"not found":          agent.ErrNotFound,
		"already decided":    agent.ErrAlreadyDecided,
		"finished":           agent.ErrFinished,
		"another sentinel":   agent.ErrConflict,
	}
	for _, r := range routes {
		for ctxName, newCtx := range contexts {
			for failure, err := range failures {
				t.Run(r.pattern+"/"+ctxName+"/"+failure, func(t *testing.T) {
					f := mutationFake().fail(r.call, err)
					method, _, _ := strings.Cut(r.pattern, " ")
					rec := doCtx(t, newCtx(t), newAPI(t, f), method, r.path, "", "X-Actor", "sam")
					assert.GreaterOrEqual(t, rec.Code, 400, "a failed call is never a success: %s", rec.Body.String())
					assert.NotEmpty(t, rec.Body.String(), "and says so")
					assert.NotEmpty(t, f.calls, "the call was made")
				})
			}
		}
	}
}

// failingBody is a request body that gives some bytes and then fails.
type failingBody struct {
	data string
	err  error
	read bool
}

func (b *failingBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, b.err
	}
	b.read = true
	return copy(p, b.data), nil
}

// A body that cannot be read to the end is a client that left, or a connection
// that broke: not a body that was wrong, and not a 400 with a warning.
func TestMutations_ABodyThatFailsToReadIsNotABadRequest(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)
			for name, err := range map[string]error{
				"a reset":              errors.New("read tcp: connection reset by peer"),
				"cut short":            io.ErrUnexpectedEOF,
				"the context's own":    context.Canceled,
				"a deadline in a read": context.DeadlineExceeded,
			} {
				for ctxName, cancelled := range map[string]bool{"cancelled": true, "live": false} {
					t.Run(name+"/"+ctxName, func(t *testing.T) {
						var sink logSink
						f := mutationFake()
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						if cancelled {
							cancel()
						}
						req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, &failingBody{data: `{"reason":"par`, err: err})
						req.Header.Set("X-Actor", "sam")
						rec := httptest.NewRecorder()
						newAPI(t, f, func(o *httpapi.Options) { o.Logger = sink.logger() }).ServeHTTP(rec, req)

						requireError(t, rec, http.StatusServiceUnavailable)
						assert.Empty(t, f.calls, "nothing was decided")
						assert.Empty(t, sink.atLeast(slog.LevelInfo), "a client leaving is not a fault")
						logged := sink.all()
						require.Len(t, logged, 1)
						assert.Equal(t, slog.LevelDebug, logged[0].Level)
					})
				}
			}
			t.Run("a body that is too long is still the client's mistake", func(t *testing.T) {
				f := mutationFake()
				rec := do(t, newAPI(t, f), http.MethodPost, path, `{"reason":"`+strings.Repeat("a", 5000)+`"}`, "X-Actor", "sam")
				requireError(t, rec, http.StatusBadRequest)
			})
		})
	}
}

// The name an actor gives is recorded for good, and comes from the mount, which
// is trusted to say who is acting and not to say it in a form that can be
// recorded. A name that is not text, or holds a control character, or is
// without a character anyone can see, or is long, is no name.
func TestMutations_RefuseAnActorNameThatCannotBeRecorded(t *testing.T) {
	longest := strings.Repeat("a", 256)
	refused := map[string]string{
		"not UTF-8":                    "sam\xff",
		"a lone continuation byte":     "\x80sam",
		"a NUL":                        "sam\x00lee",
		"a NUL at the end":             "sam\x00",
		"a line feed inside":           "sam\nlee",
		"a carriage return inside":     "sam\rlee",
		"a tab inside":                 "sam\tlee",
		"a bell":                       "sam\x07",
		"a delete":                     "sam\x7flee",
		"a next line inside":           "sam\u0085lee",
		"a line separator inside":      "sam\u2028lee",
		"a paragraph separator inside": "sam\u2029lee",
		"257 bytes":                    longest + "a",
		"257 bytes after trimming":     "  " + longest + "a  ",
		"long in multi-byte letters":   strings.Repeat("\u00e9", 129),
		"only a zero-width space":      "\u200b",
		"only a byte order mark":       "\ufeff",
		"only a soft hyphen":           "\u00ad",
		"only a zero-width joiner":     "\u200d",
		"only format characters":       "\u200b\u200d\u2060\ufeff\u00ad",
		"format characters and spaces": " \u200b \u00a0\ufeff ",
	}
	accepted := []string{
		"sam", "Sam Lee", "sam.lee@example.com", "o'brien", "\u674e\u96f7", "ops-team/3", "user 42", "+",
		"!", "42", "\u2160", "e\u0301", longest, strings.Repeat("\u00e9", 128), "\u200bsam\u200b",
	}
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			path := fmt.Sprintf(m.path, m.pending)
			for name, who := range refused {
				t.Run("refuses "+name, func(t *testing.T) {
					f := mutationFake()
					h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = func(*http.Request) string { return who } })
					rec := do(t, h, http.MethodPost, path, "")
					requireError(t, rec, http.StatusForbidden)
					assert.Empty(t, f.calls)
				})
			}
			for _, who := range accepted {
				t.Run("accepts "+strconv.Quote(who), func(t *testing.T) {
					f := mutationFake()
					h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = func(*http.Request) string { return who } })
					rec := do(t, h, http.MethodPost, path, "")
					require.Equal(t, m.status, rec.Code, rec.Body.String())
					call, ok := m.seen(f)
					require.True(t, ok)
					assert.Equal(t, who, call.By)
				})
			}
		})
	}
}

// A journal is private to whoever is allowed to see it: nothing is kept.
func TestJSONRoutes_AreNotCached(t *testing.T) {
	f := mutationFake()
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
	h := newAPI(t, f)
	for _, tt := range []struct {
		name, method, path string
	}{
		{"list of runs", http.MethodGet, "/runs"},
		{"run", http.MethodGet, "/runs/" + uid(1)},
		{"timeline", http.MethodGet, "/runs/" + uid(1) + "/timeline"},
		{"list of approvals", http.MethodGet, "/approvals"},
		{"approve", http.MethodPost, "/approvals/" + uid(50) + "/approve"},
		{"cancel", http.MethodPost, "/runs/" + uid(1) + "/cancel"},
		{"not found", http.MethodGet, "/runs/" + uid(9)},
		{"bad request", http.MethodGet, "/runs?limit=0"},
		{"refused", http.MethodPost, "/runs/" + uid(1) + "/cancel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			headers := []string{"X-Actor", "sam"}
			if tt.name == "refused" {
				headers = []string{"Sec-Fetch-Site", "cross-site"}
			}
			rec := do(t, h, tt.method, tt.path, "", headers...)
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		})
	}
}

func TestCancel_AnswersWithNoBody(t *testing.T) {
	f := mutationFake()
	rec := do(t, newAPI(t, f), http.MethodPost, "/runs/"+uid(1)+"/cancel", `{"reason":"enough"}`, "X-Actor", "sam")
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, []cancelCall{{RunID: uid(1), By: "sam", Reason: "enough"}}, f.cancels)
}

func TestDecide_AnswersWithTheApprovalAsDecided(t *testing.T) {
	for _, tt := range []struct {
		action string
		want   agent.ApprovalStatus
	}{
		{"approve", agent.ApprovalApproved},
		{"decline", agent.ApprovalDeclined},
	} {
		t.Run(tt.action, func(t *testing.T) {
			f := mutationFake()
			rec := do(t, newAPI(t, f), http.MethodPost, "/approvals/"+uid(50)+"/"+tt.action,
				`{"reason":"because"}`, "X-Actor", "sam")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			got := decodeBody[agent.Approval](t, rec)
			assert.Equal(t, uid(50), got.ID)
			assert.Equal(t, tt.want, got.Status)
			assert.Equal(t, "sam", got.DecidedBy)
			assert.Equal(t, "because", got.Reason)
			require.NotNil(t, got.DecidedAt)
		})
	}
}

// A store answers an approval already decided with the decision and an error.
// The client is told 409 and nothing of what the decision was.
func TestDecide_ConflictBodyDoesNotCarryTheApproval(t *testing.T) {
	f := mutationFake().fail("Approve", agent.ErrAlreadyDecided)
	rec := do(t, newAPI(t, f), http.MethodPost, "/approvals/"+uid(50)+"/approve", "", "X-Actor", "sam")
	requireError(t, rec, http.StatusConflict)
	assert.NotContains(t, rec.Body.String(), "somebody", "the generic body says nothing of the approval")
}

func TestReadRoutesNeedNoActor(t *testing.T) {
	f := mutationFake()
	f.edit(func(f *fakeRuns) { f.steps[uid(1)] = []agent.Step{mkStep(uid(1), 1, 2)} })
	h := newAPI(t, f, func(o *httpapi.Options) { o.Actor = nil })
	for _, path := range []string{
		"/runs", "/runs/" + uid(1), "/runs/" + uid(1) + "/timeline", "/approvals",
	} {
		assert.Equal(t, http.StatusOK, do(t, h, http.MethodGet, path, "").Code, path)
	}
}

// Failures are logged with their cause, through the logger in Options when
// nothing on the request carries another.
func TestLogger(t *testing.T) {
	t.Run("Options.Logger receives the cause of a failure", func(t *testing.T) {
		var sink logSink
		f := newFake().addRun(mkRun(uid(1))).fail("GetRun", errStoreDown)
		h := newAPI(t, f, func(o *httpapi.Options) { o.Logger = sink.logger() })
		requireError(t, do(t, h, http.MethodGet, "/runs/"+uid(1), ""), http.StatusInternalServerError)

		errs := sink.atLeast(slog.LevelError)
		require.Len(t, errs, 1)
		assert.Equal(t, "request failed", errs[0].Msg)
		assert.Contains(t, errs[0].Attrs["error"], "agent_runs", "the cause is in the log, and only there")
	})

	t.Run("the router's logger is preferred when there is one", func(t *testing.T) {
		var routerSink, optionSink logSink
		f := newFake().fail("GetRun", errStoreDown)
		api, err := httpapi.New(httpapi.Options{Runs: f, Logger: optionSink.logger()})
		require.NoError(t, err)
		router, err := httpx.NewRouter(httpx.RouterOptions{Logger: routerSink.logger(), SkipRequestLog: true})
		require.NoError(t, err)
		router.Mount("/", api.Routes())

		requireError(t, do(t, router, http.MethodGet, "/runs/"+uid(1), ""), http.StatusInternalServerError)
		assert.Len(t, routerSink.atLeast(slog.LevelError), 1)
		assert.Empty(t, optionSink.all())
	})
}
