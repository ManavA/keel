package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ManavA/keel/llm"
)

// The scripted model. It answers from the request alone: how many turns the
// conversation holds and what the tools returned. It keeps no state of its
// own, so a run that is killed and resumed by another process gets the reply
// it would have got, and two runs at once do not disturb each other.

// digestRecipient is where the scripted coordinator sends the digest.
const digestRecipient = "team@example.com"

// demoScript routes a request to the agent's script by the model it names.
func demoScript() llm.Script {
	return llm.Route(map[string]llm.Script{
		scriptedCoordinator: coordinatorScript,
		scriptedReviewer:    reviewerScript,
	})
}

// conversation counts the assistant turns in req and groups the tool results
// by the turn they answer.
func conversation(req llm.Request) (turns int, results [][]llm.ToolResult) {
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleAssistant:
			turns++
			results = append(results, nil)
		case llm.RoleTool:
			if len(results) > 0 {
				results[len(results)-1] = append(results[len(results)-1], m.ToolResults...)
			}
		}
	}
	return turns, results
}

func call(id, name string, input any) llm.ToolCall {
	raw, err := json.Marshal(input)
	if err != nil {
		raw = []byte(`{}`)
	}
	return llm.ToolCall{ID: id, Name: name, Input: raw}
}

// coordinatorScript lists the batch, delegates each document, then sends one
// digest and tries to delete the originals in the same turn, and ends: four
// model calls.
func coordinatorScript(req llm.Request, _ int) (llm.Reply, error) {
	turns, results := conversation(req)
	if turns == 0 {
		return llm.Reply{ToolCalls: []llm.ToolCall{call("list-1", toolListDocuments, struct{}{})}}, nil
	}

	ids := listedIDs(results[0])
	if len(ids) == 0 {
		return llm.Reply{Text: "There are no documents in the batch."}, nil
	}

	switch turns {
	case 1:
		calls := make([]llm.ToolCall, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, call("review-"+id, toolReviewDocument, documentInput{DocumentID: id}))
		}
		return llm.Reply{ToolCalls: calls}, nil
	case 2:
		var body strings.Builder
		for _, r := range results[1] {
			line := r.Content
			if r.IsError {
				line = "a review failed: " + r.Content
			}
			body.WriteString("- " + line + "\n")
		}
		calls := []llm.ToolCall{call("send-1", toolSendDigest, digestInput{
			To:          digestRecipient,
			Subject:     fmt.Sprintf("Digest of %d documents", len(ids)),
			Body:        body.String(),
			DocumentIDs: ids,
		})}
		for _, id := range ids {
			calls = append(calls, call("delete-"+id, toolDeleteDocument, documentInput{DocumentID: id}))
		}
		return llm.Reply{ToolCalls: calls}, nil
	default:
		sent, refused := "The digest was sent.", 0
		for i, r := range results[len(results)-1] {
			switch {
			case i == 0 && r.IsError:
				sent = "The digest was not sent."
			case i > 0 && r.IsError:
				refused++
			}
		}
		return llm.Reply{Text: fmt.Sprintf("Reviewed %d documents. %s %d deletions were refused, and the documents are kept.",
			len(ids), sent, refused)}, nil
	}
}

// listedIDs reads the document ids out of list_documents' result.
func listedIDs(results []llm.ToolResult) []string {
	if len(results) == 0 || results[0].IsError {
		return nil
	}
	var docs []Document
	if err := json.Unmarshal([]byte(results[0].Content), &docs); err != nil {
		return nil
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	return ids
}

// reviewerScript reads the one document its input names, saves a one-line
// summary, and answers with that line, which the coordinator receives as the
// result of its delegation.
func reviewerScript(req llm.Request, _ int) (llm.Reply, error) {
	turns, results := conversation(req)
	switch turns {
	case 0:
		var in documentInput
		if len(req.Messages) == 0 || !parsed(req.Messages[0].Text, &in) || in.DocumentID == "" {
			return llm.Reply{Text: "The request names no document."}, nil
		}
		return llm.Reply{ToolCalls: []llm.ToolCall{call("read-"+in.DocumentID, toolReadDocument, in)}}, nil
	case 1:
		var doc Document
		if len(results[0]) == 0 || results[0][0].IsError || !parsed(results[0][0].Content, &doc) {
			return llm.Reply{Text: "The document could not be read."}, nil
		}
		return llm.Reply{ToolCalls: []llm.ToolCall{call("save-"+doc.ID, toolSaveSummary, summaryInput{
			DocumentID: doc.ID,
			Summary:    doc.Title + ": " + firstSentence(doc.Body),
		})}}, nil
	default:
		// The line it saved is in its own last call.
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
				continue
			}
			var in summaryInput
			if json.Unmarshal(m.ToolCalls[0].Input, &in) == nil && in.Summary != "" {
				return llm.Reply{Text: in.DocumentID + ": " + in.Summary}, nil
			}
		}
		return llm.Reply{Text: "The summary could not be saved."}, nil
	}
}

// parsed reports whether text is JSON that v takes. Text that is not is
// something the script answers in words, not a failed call.
func parsed(text string, v any) bool {
	return json.Unmarshal([]byte(text), v) == nil
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
