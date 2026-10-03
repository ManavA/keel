package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/httpapi"
	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/llm"
	keellog "github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
)

// An example nobody runs stops working quietly. These tests run the service
// the way its README says to, against a real database and the scripted model:
// a batch is started, waits for a person, is approved and completes.

// The surface and the engine cannot drift apart unnoticed.
var _ httpapi.Runs = (*agent.Engine)(nil)

const testToken = "test-operator-token"

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// testService is the example as run() builds it, with its own schema, served
// by httptest and with the worker running beside it.
type testService struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
}

func newTestService(t *testing.T) *testService {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "agentdemo_" + randomSuffix(t)

	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	cfg := Config{
		Config: app.Config{
			Port:              8080,
			Env:               "development",
			DatabaseURL:       db.URL + "&search_path=" + schema,
			ShutdownTimeout:   4 * time.Second,
			ReadinessCacheTTL: time.Millisecond,
		},
		MigrateOnStart: true,
		OperatorToken:  testToken,
		LeaseTTL:       5 * time.Second,
		PollInterval:   20 * time.Millisecond,
		StepDelay:      0,
	}
	require.NoError(t, cfg.Validate())

	svc, err := newService(ctx, cfg, slog.Default())
	require.NoError(t, err)

	workCtx, stop := context.WithCancel(ctx)
	worked := make(chan struct{})
	go func() {
		defer close(worked)
		_ = svc.engine.Work(workCtx)
	}()

	srv := httptest.NewServer(svc.app.Router())
	t.Cleanup(func() {
		srv.Close()
		stop()
		<-worked
		svc.app.Close()
	})
	return &testService{srv: srv, pool: svc.app.Pool()}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}

// do makes a request, with the operator token unless token is empty, and
// decodes a JSON reply into out when out is not nil.
func (ts *testService) do(t *testing.T, method, path, token string, header http.Header, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, ts.srv.URL+path, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.Unmarshal(body, out), "body: %s", body)
	}
	return resp.StatusCode
}

// awaitRun polls the run until done says it has got there.
func (ts *testService) awaitRun(t *testing.T, id string, done func(agent.Run) bool) agent.Run {
	t.Helper()
	var run agent.Run
	require.Eventually(t, func() bool {
		if ts.do(t, http.MethodGet, "/agent/runs/"+id, testToken, nil, &run) != http.StatusOK {
			return false
		}
		return done(run)
	}, 30*time.Second, 25*time.Millisecond, "run did not get there; last seen %s (%s) %s", run.Status, run.Reason, run.Error)
	return run
}

type timeline struct {
	Run       agent.Run        `json:"run"`
	Steps     []agent.Step     `json:"steps"`
	Approvals []agent.Approval `json:"approvals"`
}

const (
	ruleAsk   = "A digest of more than two documents sent outside needs a person"
	ruleBlock = "Documents are never deleted by an agent"
)

func TestBatchWaitsForApprovalThenCompletes(t *testing.T) {
	ts := newTestService(t)
	ctx := context.Background()

	// Without the token nothing is served.
	assert.Equal(t, http.StatusUnauthorized, ts.do(t, http.MethodPost, "/api/batches", "", nil, nil))
	assert.Equal(t, http.StatusUnauthorized, ts.do(t, http.MethodPost, "/api/batches", "wrong", nil, nil))
	assert.Equal(t, http.StatusUnauthorized, ts.do(t, http.MethodGet, "/agent/runs", "", nil, nil))

	// With it, a run is started; the same key gives the same run.
	key := http.Header{"Idempotency-Key": {"batch-1"}}
	var run, again agent.Run
	require.Equal(t, http.StatusAccepted, ts.do(t, http.MethodPost, "/api/batches", testToken, key, &run))
	require.Equal(t, http.StatusAccepted, ts.do(t, http.MethodPost, "/api/batches", testToken, key, &again))
	require.NotEmpty(t, run.ID)
	assert.Equal(t, agentCoordinator, run.Agent)
	assert.Equal(t, run.ID, again.ID)

	// It reaches the approval: the digest, with the rule that asked.
	ts.awaitRun(t, run.ID, func(r agent.Run) bool {
		return r.Status == agent.StatusWaiting && r.Reason == agent.ReasonApproval
	})
	var pending struct {
		Approvals []agent.Approval `json:"approvals"`
	}
	require.Equal(t, http.StatusOK, ts.do(t, http.MethodGet, "/agent/approvals?status=pending", testToken, nil, &pending))
	require.Len(t, pending.Approvals, 1)
	approval := pending.Approvals[0]
	assert.Equal(t, run.ID, approval.RunID)
	assert.Equal(t, toolSendDigest, approval.Tool)
	assert.Equal(t, ruleAsk, approval.Rule)
	assert.Equal(t, "email:"+digestRecipient, approval.Action.Target)

	// Nothing has been sent while it waits.
	var digests int
	require.NoError(t, ts.pool.QueryRow(ctx, `select count(*) from digests`).Scan(&digests))
	assert.Zero(t, digests)

	// Approved, it completes.
	var decided agent.Approval
	require.Equal(t, http.StatusOK,
		ts.do(t, http.MethodPost, "/agent/approvals/"+approval.ID+"/approve", testToken, nil, &decided))
	assert.Equal(t, agent.ApprovalApproved, decided.Status)
	assert.Equal(t, operatorName, decided.DecidedBy)

	final := ts.awaitRun(t, run.ID, func(r agent.Run) bool { return r.Terminal() })
	require.Equal(t, agent.StatusCompleted, final.Status, final.Error)
	assert.Contains(t, final.Output, "The digest was sent.")
	assert.Positive(t, final.Usage.CostMicros)

	// The timeline shows every delete blocked, by the rule in policy.json,
	// and the digest completed.
	var tl timeline
	require.Equal(t, http.StatusOK, ts.do(t, http.MethodGet, "/agent/runs/"+run.ID+"/timeline", testToken, nil, &tl))
	deletes := 0
	for _, step := range tl.Steps {
		switch step.Name {
		case toolDeleteDocument:
			deletes++
			assert.Equal(t, agent.StepBlocked, step.Status)
			assert.Equal(t, ruleBlock, step.Rule)
		case toolSendDigest:
			assert.Equal(t, agent.StepCompleted, step.Status)
		}
	}
	assert.Equal(t, 3, deletes)

	// One summary per document, one digest, and the documents still there.
	var summaries, documents int
	require.NoError(t, ts.pool.QueryRow(ctx, `select count(*) from summaries`).Scan(&summaries))
	require.NoError(t, ts.pool.QueryRow(ctx, `select count(*) from digests`).Scan(&digests))
	require.NoError(t, ts.pool.QueryRow(ctx, `select count(*) from documents`).Scan(&documents))
	assert.Equal(t, 3, summaries)
	assert.Equal(t, 1, digests)
	assert.Equal(t, 3, documents)

	// The event stream of the finished run delivers the run and ends.
	req, err := http.NewRequest(http.MethodGet, ts.srv.URL+"/agent/runs/"+run.ID+"/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "event:"); ok {
			events = append(events, strings.TrimSpace(name))
		}
	}
	require.NoError(t, scanner.Err())
	assert.Contains(t, events, "run")
	require.NotEmpty(t, events)
	assert.Equal(t, "end", events[len(events)-1])

	assert.Equal(t, http.StatusOK, ts.do(t, http.MethodGet, "/readyz", "", nil, nil))
}

func TestConfigValidate(t *testing.T) {
	valid := func() Config {
		return Config{
			Config:        app.Config{Port: 8080, DatabaseURL: "postgres://localhost/keel"},
			OperatorToken: "token",
			LeaseTTL:      time.Second,
			PollInterval:  time.Second,
		}
	}
	cases := []struct {
		name    string
		change  func(*Config)
		wantErr string
	}{
		{name: "scripted by default", change: func(*Config) {}},
		{name: "a key alone selects anthropic", change: func(c *Config) {
			c.AnthropicAPIKey, c.CoordinatorModel, c.ReviewerModel = "k", "a", "b"
		}},
		{name: "operator token missing", change: func(c *Config) { c.OperatorToken = " " }, wantErr: "OPERATOR_TOKEN"},
		{name: "unknown provider", change: func(c *Config) { c.LLMProvider = "other" }, wantErr: "LLM_PROVIDER"},
		{name: "anthropic without a key", change: func(c *Config) { c.LLMProvider = "anthropic" }, wantErr: "ANTHROPIC_API_KEY"},
		{name: "openai without a model", change: func(c *Config) { c.LLMProvider = "openai" }, wantErr: "OPENAI_MODEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.change(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	withKey := valid()
	withKey.AnthropicAPIKey = "k"
	assert.Equal(t, providerAnthropic, withKey.Provider())
	assert.Equal(t, providerScripted, valid().Provider())
}

// TestWorkerWait holds run to waiting as long as Work can take to return
// after a drain, whatever SHUTDOWN_TIMEOUT is.
func TestWorkerWait(t *testing.T) {
	for _, shutdown := range []time.Duration{time.Second, 4 * time.Second, 10 * time.Second, 20 * time.Second, time.Minute} {
		cfg := Config{Config: app.Config{ShutdownTimeout: shutdown}}
		wait := workerWait(cfg)
		assert.GreaterOrEqual(t, wait, drainTimeout(cfg)+workerLastWrites, "shutdown timeout %s", shutdown)
		assert.GreaterOrEqual(t, wait, shutdown, "shutdown timeout %s", shutdown)
	}
	assert.Equal(t, 20*time.Second, workerWait(Config{Config: app.Config{ShutdownTimeout: 20 * time.Second}}))
	assert.Equal(t, 7*time.Second, workerWait(Config{Config: app.Config{ShutdownTimeout: 4 * time.Second}}))
}

// TestCoveredDocuments holds the digest's documents attribute to what the
// digest covers, whatever list the model wrote.
func TestCoveredDocuments(t *testing.T) {
	batch := []Document{
		{ID: "doc-1", Title: "Quarterly plan"},
		{ID: "doc-2", Title: "Incident review"},
		{ID: "doc-3", Title: "Hiring update"},
	}
	cases := []struct {
		name string
		args digestInput
		want int
	}{
		{name: "the ids it lists", args: digestInput{DocumentIDs: []string{"doc-1", "doc-2", "doc-3"}}, want: 3},
		{name: "an id listed twice counts once", args: digestInput{DocumentIDs: []string{"doc-1", "doc-1", "doc-1"}}, want: 1},
		{name: "an id not in the batch is not counted", args: digestInput{DocumentIDs: []string{"doc-1", "doc-9"}}, want: 1},
		{
			name: "fewer ids than the body covers counts the body",
			args: digestInput{
				DocumentIDs: []string{"doc-1"},
				Body:        "- Quarterly plan: three goals.\n- incident review: an index.\n- Hiring update: two offers.\n",
			},
			want: 3,
		},
		{name: "an id in the subject", args: digestInput{Subject: "About doc-2", DocumentIDs: []string{}}, want: 1},
		{name: "nothing", args: digestInput{Body: "nothing to report"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, coveredDocuments(batch, tc.args))
		})
	}
}

// TestScript holds the scripted model to its replies: for a request as the
// engine sends it at each turn, the calls expected, with the document ids
// taken from the request and not from anything the script remembers.
func TestScript(t *testing.T) {
	user := func(text string) llm.Message { return llm.Message{Role: llm.RoleUser, Text: text} }
	asked := func(calls ...llm.ToolCall) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}
	}
	answered := func(results ...llm.ToolResult) llm.Message {
		return llm.Message{Role: llm.RoleTool, ToolResults: results}
	}
	listed := answered(llm.ToolResult{CallID: "list-1", Content: `[{"id":"a-1","title":"A"},{"id":"b-2","title":"B"}]`})
	reviewed := answered(
		llm.ToolResult{CallID: "review-a-1", Content: "a-1: A: first."},
		llm.ToolResult{CallID: "review-b-2", Content: "b-2: B: second."},
	)
	saved := call("save-a-1", toolSaveSummary, summaryInput{DocumentID: "a-1", Summary: "A: One."})

	cases := []struct {
		name     string
		model    string
		messages []llm.Message
		want     []string // "tool input" per call, in order
		wantText string
	}{
		{
			name: "coordinator lists first", model: scriptedCoordinator,
			messages: []llm.Message{user(batchInput)},
			want:     []string{`list_documents {}`},
		},
		{
			name: "coordinator delegates each listed document", model: scriptedCoordinator,
			messages: []llm.Message{user(batchInput), asked(), listed},
			want:     []string{`review_document {"document_id":"a-1"}`, `review_document {"document_id":"b-2"}`},
		},
		{
			name: "coordinator sends one digest and tries the deletes", model: scriptedCoordinator,
			messages: []llm.Message{user(batchInput), asked(), listed, asked(), reviewed},
			want: []string{
				`send_digest {"to":"team@example.com","subject":"Digest of 2 documents","body":"- a-1: A: first.\n- b-2: B: second.\n","document_ids":["a-1","b-2"]}`,
				`delete_document {"document_id":"a-1"}`,
				`delete_document {"document_id":"b-2"}`,
			},
		},
		{
			name: "coordinator ends by saying what happened", model: scriptedCoordinator,
			messages: []llm.Message{user(batchInput), asked(), listed, asked(), reviewed, asked(), answered(
				llm.ToolResult{CallID: "send-1", Content: "sent"},
				llm.ToolResult{CallID: "delete-a-1", Content: "blocked", IsError: true},
				llm.ToolResult{CallID: "delete-b-2", Content: "blocked", IsError: true},
			)},
			wantText: "Reviewed 2 documents. The digest was sent. 2 deletions were refused, and the documents are kept.",
		},
		{
			name: "reviewer reads the document its input names", model: scriptedReviewer,
			messages: []llm.Message{user(`{"document_id":"a-1"}`)},
			want:     []string{`read_document {"document_id":"a-1"}`},
		},
		{
			name: "reviewer saves a line made from what it read", model: scriptedReviewer,
			messages: []llm.Message{user(`{"document_id":"a-1"}`), asked(),
				answered(llm.ToolResult{CallID: "read-a-1", Content: `{"id":"a-1","title":"A","body":"One. Two."}`})},
			want: []string{`save_summary {"document_id":"a-1","summary":"A: One."}`},
		},
		{
			name: "reviewer answers with the line it saved", model: scriptedReviewer,
			messages: []llm.Message{user(`{"document_id":"a-1"}`), asked(), answered(), asked(saved),
				answered(llm.ToolResult{CallID: "save-a-1", Content: "saved"})},
			wantText: "a-1: A: One.",
		},
	}
	script := demoScript()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := script(llm.Request{Model: tc.model, Messages: tc.messages}, 0)
			require.NoError(t, err)
			var got []string
			for _, c := range reply.ToolCalls {
				got = append(got, c.Name+" "+string(c.Input))
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantText, reply.Text)
		})
	}
}
