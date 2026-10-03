package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ManavA/keel/agent"
)

// The agents and their tools, by name.
const (
	agentCoordinator = "coordinator"
	agentReviewer    = "reviewer"

	toolListDocuments  = "list_documents"
	toolReviewDocument = "review_document"
	toolSendDigest     = "send_digest"
	toolDeleteDocument = "delete_document"
	toolReadDocument   = "read_document"
	toolSaveSummary    = "save_summary"
)

const coordinatorSystem = `You review a batch of documents and report on it.
Call list_documents. Then call review_document once for each document. Then call
send_digest once, with one line per document taken from the reviews, and call
delete_document for each document. If a call is refused or declined, do not make
it again: finish by saying in a sentence or two what was done and what was not.`

const reviewerSystem = `You are given one document id as JSON. Call
read_document for it, then call save_summary with a one-line summary of the
document, then answer with the document id, a colon and that line.`

// What the model writes as a tool's arguments.
type documentInput struct {
	DocumentID string `json:"document_id"`
}

type summaryInput struct {
	DocumentID string `json:"document_id"`
	Summary    string `json:"summary"`
}

type digestInput struct {
	To          string   `json:"to"`
	Subject     string   `json:"subject"`
	Body        string   `json:"body"`
	DocumentIDs []string `json:"document_ids"`
}

const (
	documentSchema = `{"type":"object","properties":{"document_id":{"type":"string"}},"required":["document_id"],"additionalProperties":false}`
	summarySchema  = `{"type":"object","properties":{"document_id":{"type":"string"},"summary":{"type":"string"}},"required":["document_id","summary"],"additionalProperties":false}`
	digestSchema   = `{"type":"object","properties":{"to":{"type":"string"},"subject":{"type":"string"},"body":{"type":"string"},"document_ids":{"type":"array","items":{"type":"string"}}},"required":["to","subject","body","document_ids"],"additionalProperties":false}`
	noInputSchema  = `{"type":"object","properties":{},"additionalProperties":false}`
)

// decodeInput reads a tool call's arguments, which a model wrote. Numbers are
// kept as json.Number: one decoded into a float64 has already rounded, and
// policy compares what it is given exactly. The inputs here carry no number a
// rule reads (the count a rule reads is taken in Go, below), but a tool that
// puts a model-written amount into its action decodes it this way.
func decodeInput(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}

// pause sleeps for STEP_DELAY, so that a run takes long enough to be killed
// half way. It ends early when the call's context does.
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", agent.ErrTransient, ctx.Err())
	case <-t.C:
		return nil
	}
}

// definitions are the two agents. Every process that may resume a run
// registers both, with these names.
func definitions(docs *Documents, names modelNames, cfg Config) []agent.Definition {
	limits := agent.Limits{MaxCostMicros: cfg.RunMaxCostMicros, MaxModelCalls: 12}
	delay := cfg.StepDelay

	coordinator := agent.Definition{
		Name:      agentCoordinator,
		System:    coordinatorSystem,
		Model:     names.coordinator,
		MaxTokens: maxReplyTokens,
		Limits:    limits,
		Tools: []agent.Tool{
			listDocuments(docs, delay),
			{
				// A delegating tool starts a child run of the reviewer with the
				// call's arguments as its input, and the child's answer is the
				// result. The engine describes it to the guard as a "delegate"
				// action on the agent's name.
				Name:        toolReviewDocument,
				Description: "Have one document reviewed and summarised. Returns the document id and its one-line summary.",
				Schema:      json.RawMessage(documentSchema),
				Delegate:    agentReviewer,
			},
			sendDigest(docs, delay),
			deleteDocument(docs, delay),
		},
	}
	reviewer := agent.Definition{
		Name:      agentReviewer,
		System:    reviewerSystem,
		Model:     names.reviewer,
		MaxTokens: maxReplyTokens,
		Limits:    limits,
		Tools: []agent.Tool{
			readDocument(docs, delay),
			saveSummary(docs, delay),
		},
	}
	return []agent.Definition{coordinator, reviewer}
}

func listDocuments(docs *Documents, delay time.Duration) agent.Tool {
	return agent.Tool{
		Name:        toolListDocuments,
		Description: "List the documents in the batch: their ids and titles, as JSON.",
		Schema:      json.RawMessage(noInputSchema),
		Action: func(agent.Invocation) agent.Action {
			return agent.Action{Kind: kindRead, Target: targetBatch}
		},
		Run: func(ctx context.Context, _ agent.Invocation) (string, error) {
			if err := pause(ctx, delay); err != nil {
				return "", err
			}
			list, err := docs.List(ctx)
			if err != nil {
				return "", transient(err)
			}
			out, err := json.Marshal(list)
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

func readDocument(docs *Documents, delay time.Duration) agent.Tool {
	return agent.Tool{
		Name:        toolReadDocument,
		Description: "Read one document: its id, title and body, as JSON.",
		Schema:      json.RawMessage(documentSchema),
		Action: func(in agent.Invocation) agent.Action {
			var args documentInput
			_ = decodeInput(in.Call.Input, &args) // an id that did not decode is the invalid target
			return agent.Action{Kind: kindRead, Target: documentTarget(args.DocumentID)}
		},
		Run: func(ctx context.Context, in agent.Invocation) (string, error) {
			var args documentInput
			if err := decodeInput(in.Call.Input, &args); err != nil {
				return "", err
			}
			if !validDocumentID(args.DocumentID) {
				return "", errors.New("document_id is not a document id")
			}
			if err := pause(ctx, delay); err != nil {
				return "", err
			}
			doc, err := docs.Get(ctx, args.DocumentID)
			if errors.Is(err, errNoDocument) {
				return "", err
			}
			if err != nil {
				return "", transient(err)
			}
			out, err := json.Marshal(doc)
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

func saveSummary(docs *Documents, delay time.Duration) agent.Tool {
	return agent.Tool{
		Name:        toolSaveSummary,
		Description: "Save the one-line summary of a document.",
		Schema:      json.RawMessage(summarySchema),
		Action: func(in agent.Invocation) agent.Action {
			var args summaryInput
			_ = decodeInput(in.Call.Input, &args)
			return agent.Action{Kind: kindWrite, Target: summaryTarget(args.DocumentID)}
		},
		Run: func(ctx context.Context, in agent.Invocation) (string, error) {
			var args summaryInput
			if err := decodeInput(in.Call.Input, &args); err != nil {
				return "", err
			}
			if !validDocumentID(args.DocumentID) {
				return "", errors.New("document_id is not a document id")
			}
			if strings.TrimSpace(args.Summary) == "" {
				return "", errors.New("summary must not be empty")
			}
			if err := pause(ctx, delay); err != nil {
				return "", err
			}
			// in.Key is the same on every attempt at this call, which is what
			// makes the row happen once however often the process is killed.
			if err := docs.SaveSummary(ctx, in.Key, in.RunID, args.DocumentID, args.Summary); err != nil {
				return "", transient(err)
			}
			return "saved", nil
		},
	}
}

func sendDigest(docs *Documents, delay time.Duration) agent.Tool {
	return agent.Tool{
		Name:        toolSendDigest,
		Description: "Send one digest of the batch to an address. document_ids lists every document it covers.",
		Schema:      json.RawMessage(digestSchema),
		Action: func(in agent.Invocation) agent.Action {
			var args digestInput
			if err := decodeInput(in.Call.Input, &args); err != nil {
				// No attributes: no rule allows a send without them, so
				// arguments that cannot be read are blocked by default.
				return agent.Action{Kind: kindSend, Target: "email:invalid"}
			}
			attrs := map[string]any{
				// Every address is outside this example; a service with
				// addresses of its own decides this from the address.
				attrExternal: true,
			}
			// How many documents the digest covers is counted here, from the
			// batch, and not taken from the list the model wrote. A batch
			// that cannot be read leaves the attribute out, and no rule
			// allows a send without it.
			ctx, cancel := context.WithTimeout(context.Background(), batchReadTimeout)
			batch, err := docs.List(ctx)
			cancel()
			if err == nil {
				attrs[attrDocuments] = coveredDocuments(batch, args)
			}
			if rule, matched := textRule(args.Subject, args.Body); matched {
				attrs[attrTextRule] = rule
			}
			return agent.Action{Kind: kindSend, Target: emailTarget(args.To), Attrs: attrs}
		},
		Run: func(ctx context.Context, in agent.Invocation) (string, error) {
			var args digestInput
			if err := decodeInput(in.Call.Input, &args); err != nil {
				return "", err
			}
			to, ok := canonicalAddress(args.To)
			if !ok {
				return "", errors.New("to is not an address")
			}
			if len(args.DocumentIDs) == 0 {
				return "", errors.New("document_ids must name at least one document")
			}
			if err := pause(ctx, delay); err != nil {
				return "", err
			}
			if err := docs.RecordDigest(ctx, in.Key, in.RunID, to, args.Subject, args.Body, args.DocumentIDs); err != nil {
				return "", transient(err)
			}
			return fmt.Sprintf("digest of %d documents sent to %s", len(args.DocumentIDs), to), nil
		},
	}
}

func deleteDocument(docs *Documents, delay time.Duration) agent.Tool {
	return agent.Tool{
		Name:        toolDeleteDocument,
		Description: "Delete one document from the batch.",
		Schema:      json.RawMessage(documentSchema),
		Action: func(in agent.Invocation) agent.Action {
			var args documentInput
			_ = decodeInput(in.Call.Input, &args)
			return agent.Action{Kind: kindDelete, Target: documentTarget(args.DocumentID)}
		},
		Run: func(ctx context.Context, in agent.Invocation) (string, error) {
			var args documentInput
			if err := decodeInput(in.Call.Input, &args); err != nil {
				return "", err
			}
			if !validDocumentID(args.DocumentID) {
				return "", errors.New("document_id is not a document id")
			}
			if err := pause(ctx, delay); err != nil {
				return "", err
			}
			if err := docs.Delete(ctx, in.Key, args.DocumentID); err != nil {
				return "", transient(err)
			}
			return "deleted", nil
		},
	}
}

// transient marks a database failure as worth another attempt: the call stays
// started and is made again later with the same key, instead of the model
// being told the tool failed.
func transient(err error) error {
	return fmt.Errorf("%w: %w", agent.ErrTransient, err)
}

// batchReadTimeout bounds the read of the batch a digest's action is
// described from.
const batchReadTimeout = 5 * time.Second

// coveredDocuments counts the documents of batch a digest covers: those it
// names by id, and those whose id or title its subject or body mentions. A
// model that lists fewer ids than its text covers is counted by its text,
// and an id listed twice or not in the batch is not counted.
func coveredDocuments(batch []Document, args digestInput) int {
	listed := make(map[string]bool, len(args.DocumentIDs))
	for _, id := range args.DocumentIDs {
		listed[id] = true
	}
	text := strings.ToLower(args.Subject + "\n" + args.Body)
	covered := 0
	for _, doc := range batch {
		mentioned := strings.Contains(text, strings.ToLower(doc.ID)) ||
			(doc.Title != "" && strings.Contains(text, strings.ToLower(doc.Title)))
		if listed[doc.ID] || mentioned {
			covered++
		}
	}
	return covered
}
