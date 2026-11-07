package session

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// legacyExtractSessionInfo copies the parent-candidate block and branches 1-2 of
// ExtractSessionInfo as they were before the optimization (calling
// ClaudeMetadataParentSessionID and ClaudeMetadataIdentities on the raw payload) and
// delegates the unchanged remainder to ExtractSessionInfo. Update it if those
// sections change.
func legacyExtractSessionInfo(headers http.Header, payload []byte, metadata map[string]any) (SessionInfo, bool) {
	var info SessionInfo
	if metadata != nil {
		if scope, ok := metadata[cliproxyexecutor.CallerScopeMetadataKey].(string); ok {
			info.CallerScope = strings.TrimSpace(scope)
		}
	}

	var root sessionObject
	var reqRoot sessionObject
	var hasNestedReq bool
	var parentCandidate string

	if len(payload) > 0 {
		root = newSessionObject(util.ParseGJSONBytesNoCopy(payload))
		reqRoot = root
		req := root.Get("request")
		hasNestedReq = req.Exists() && !root.Get("contents").Exists()
		if hasNestedReq {
			reqRoot = newSessionObject(req)
		}
		for _, p := range []string{
			"parent_session_id", "parentSessionId", "parentSessionID",
			"parent_thread_id", "parentThreadId", "parentThreadID",
			"forked_from_thread_id", "forked_from_id",
			"parent_conversation_id", "parentConversationId", "parentConversationID",
			"parent_id", "parentId", "parentID",
			"parent_task_id", "parentTaskId", "parentTaskID",
			"parent_action_id", "parentActionId", "parentActionID",
			"parent_session", "parentSession",
			"parent_subagent_id", "parentSubagentId",
			"forkSource.sessionId", "fork_source.session_id",
			"previousSessionId", "previous_session_id",
			"metadata.parent_session_id", "metadata.parentSessionId", "metadata.parentSessionID",
			"metadata.parent_thread_id", "metadata.parentThreadId",
			"metadata.forked_from_thread_id", "metadata.forked_from_id",
			"metadata.parent_id", "metadata.parentId", "metadata.parentID",
			"metadata.parent_task_id", "metadata.parentTaskId", "metadata.parentTaskID",
			"metadata.parent_action_id", "metadata.parentActionId",
			"metadata.parent_subagent_id", "metadata.parentSubagentId",
			"metadata.parent_session", "metadata.parentSession",
			"metadata.parent_agent_id", "metadata.parentAgentId",
			"metadata.forkSource.sessionId", "metadata.previousSessionId",
			"extra_body.parent_session_id", "extra_body.parentSessionId", "extra_body.parentSessionID",
			"extra_body.parent_thread_id", "extra_body.parentThreadId",
			"extra_body.forked_from_thread_id", "extra_body.forked_from_id",
			"extra_body.parent_id", "extra_body.parentId", "extra_body.parentID",
			"extra_body.parent_task_id", "extra_body.parentTaskId",
			"extra_body.parent_action_id", "extra_body.parentActionId",
			"extra_body.parent_subagent_id", "extra_body.parentSubagentId",
			"extra_body.parent_session", "extra_body.parentSession",
		} {
			if val := normalizedSessionCandidate(root.Get(p).String()); val != "" {
				parentCandidate = val
				break
			}
			if hasNestedReq {
				if val := normalizedSessionCandidate(reqRoot.Get(p).String()); val != "" {
					parentCandidate = val
					break
				}
			}
		}
		if parentCandidate == "" {
			parentCandidate = ClaudeMetadataParentSessionID(payload)
		}
	}

	// 1. Anthropic / Claude Code Headers
	if sid := sessionHeaderValue(headers, "X-Claude-Code-Session-Id"); sid != "" {
		info.ClientType = "claude"
		agentID := sessionHeaderValue(headers, "X-Claude-Code-Agent-Id")
		if agentID == "" && root.Exists() {
			agentID = normalizedSessionCandidate(root.Get("metadata.agent_id").String())
			if agentID == "" {
				agentID = normalizedSessionCandidate(root.Get("metadata.subagent_id").String())
			}
			if agentID == "" && hasNestedReq {
				agentID = normalizedSessionCandidate(reqRoot.Get("metadata.agent_id").String())
				if agentID == "" {
					agentID = normalizedSessionCandidate(reqRoot.Get("metadata.subagent_id").String())
				}
			}
		}
		if agentID == "" {
			_, _, agentID = ClaudeMetadataIdentities(payload)
		}
		parentAgentID := sessionHeaderValue(headers, "X-Claude-Code-Parent-Agent-Id")
		if parentAgentID == "" && root.Exists() {
			parentAgentID = normalizedSessionCandidate(root.Get("metadata.parent_agent_id").String())
			if parentAgentID == "" {
				parentAgentID = normalizedSessionCandidate(root.Get("metadata.parentAgentId").String())
			}
			if parentAgentID == "" && hasNestedReq {
				parentAgentID = normalizedSessionCandidate(reqRoot.Get("metadata.parent_agent_id").String())
				if parentAgentID == "" {
					parentAgentID = normalizedSessionCandidate(reqRoot.Get("metadata.parentAgentId").String())
				}
			}
		}
		if agentID != "" && agentID != "main" {
			info.AgentName = agentID
			info.ParentSessionID = "claude:" + sid
			if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
				info.ParentSessionID = "claude:" + sid + ":agent:" + parentAgentID
			} else if parentCandidate != "" && parentCandidate != sid {
				info.ParentSessionID = "claude:" + parentCandidate
			}
			info.SessionID = "claude:" + sid + ":agent:" + agentID
		} else {
			info.AgentName = "main"
			info.SessionID = "claude:" + sid
			if parentCandidate != "" && parentCandidate != sid {
				info.ParentSessionID = "claude:" + parentCandidate
				info.AgentName = "subagent"
			}
		}
		return finalizeSessionInfo(info)
	}

	// 2. Claude Code metadata.user_id in payload (outranks generic headers)
	if len(payload) > 0 {
		if sid, parentSID, agentID := ClaudeMetadataIdentities(payload); sid != "" {
			info.ClientType = "claude"
			if agentID == "" {
				agentID = sessionHeaderValue(headers, "X-Claude-Code-Agent-Id")
			}
			if agentID == "" && root.Exists() {
				agentID = normalizedSessionCandidate(root.Get("metadata.agent_id").String())
				if agentID == "" {
					agentID = normalizedSessionCandidate(root.Get("metadata.subagent_id").String())
				}
				if agentID == "" && hasNestedReq {
					agentID = normalizedSessionCandidate(reqRoot.Get("metadata.agent_id").String())
					if agentID == "" {
						agentID = normalizedSessionCandidate(reqRoot.Get("metadata.subagent_id").String())
					}
				}
			}
			parentAgentID := sessionHeaderValue(headers, "X-Claude-Code-Parent-Agent-Id")
			if parentAgentID == "" && root.Exists() {
				parentAgentID = normalizedSessionCandidate(root.Get("metadata.parent_agent_id").String())
				if parentAgentID == "" {
					parentAgentID = normalizedSessionCandidate(root.Get("metadata.parentAgentId").String())
				}
				if parentAgentID == "" && hasNestedReq {
					parentAgentID = normalizedSessionCandidate(reqRoot.Get("metadata.parent_agent_id").String())
					if parentAgentID == "" {
						parentAgentID = normalizedSessionCandidate(reqRoot.Get("metadata.parentAgentId").String())
					}
				}
			}
			if agentID != "" && agentID != "main" {
				info.SessionID = "claude:" + sid + ":agent:" + agentID
				info.ParentSessionID = "claude:" + sid
				if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
					info.ParentSessionID = "claude:" + sid + ":agent:" + parentAgentID
				} else if parentSID != "" && parentSID != sid {
					info.ParentSessionID = "claude:" + parentSID
				} else if parentCandidate != "" && parentCandidate != sid {
					info.ParentSessionID = "claude:" + parentCandidate
				}
				info.AgentName = agentID
			} else {
				info.SessionID = "claude:" + sid
				if parentSID != "" && parentSID != sid {
					info.ParentSessionID = "claude:" + parentSID
					info.AgentName = "subagent"
				} else if parentCandidate != "" && parentCandidate != sid {
					info.ParentSessionID = "claude:" + parentCandidate
					info.AgentName = "subagent"
				} else {
					info.AgentName = "main"
				}
			}
			return finalizeSessionInfo(info)
		}
	}

	// For the rest of extraction, call the shared path since lines 3..12 are identical.
	return ExtractSessionInfo(headers, payload, metadata)
}

func TestExtractSessionInfoEquivalenceWithPreChangeLogic(t *testing.T) {
	testCases := []struct {
		name     string
		headers  http.Header
		payload  []byte
		metadata map[string]any
	}{
		{
			name:    "empty payload without headers",
			headers: nil,
			payload: nil,
		},
		{
			name:    "empty byte slice payload",
			headers: nil,
			payload: []byte{},
		},
		{
			name:    "Claude JSON user_id with session/parent/agent IDs",
			headers: nil,
			payload: []byte(`{"model":"claude-3-7-sonnet","metadata":{"user_id":"{\"session_id\":\"s-123\",\"parent_session_id\":\"p-456\",\"agent_id\":\"a-789\"}"}}`),
		},
		{
			name:    "Claude JSON user_id with main agent",
			headers: nil,
			payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"s-main\",\"parent_session_id\":\"p-parent\",\"agent_id\":\"main\"}"}}`),
		},
		{
			name:    "Claude JSON user_id with subagent fallback keys",
			headers: nil,
			payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"s-fallback\",\"parent_id\":\"p-id\",\"subagent_id\":\"sub-1\"}"}}`),
		},
		{
			name:    "legacy string user_id with separate metadata agent fields",
			headers: nil,
			payload: []byte(`{"metadata":{"user_id":"user_aaa_account_bbb_session_leg-sess","parent_agent_id":"p-agent","agent_id":"my-agent"}}`),
		},
		{
			name:    "nested request.metadata.user_id without top-level contents",
			headers: nil,
			payload: []byte(`{"request":{"metadata":{"user_id":"{\"session_id\":\"s-nested\",\"parent_session_id\":\"p-nested\",\"agent_id\":\"a-nested\"}"}}}`),
		},
		{
			name:    "nested request.metadata.user_id with top-level contents (must not win)",
			headers: nil,
			payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"request":{"metadata":{"user_id":"{\"session_id\":\"s-nested\"}"}}}`),
		},
		{
			name:    "X-Claude-Code-Session-Id header with metadata agent IDs",
			headers: http.Header{"X-Claude-Code-Session-Id": {"hdr-sess"}},
			payload: []byte(`{"metadata":{"agent_id":"agent-from-meta","parent_agent_id":"parent-from-meta"}}`),
		},
		{
			name:    "X-Claude-Code-Session-Id header with agent from user_id",
			headers: http.Header{"X-Claude-Code-Session-Id": {"hdr-sess"}},
			payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"ignored-sid\",\"agent_id\":\"agent-from-uid\"}"}}`),
		},
		{
			name:    "X-Claude-Code-Session-Id header with both header and metadata agent IDs",
			headers: http.Header{"X-Claude-Code-Session-Id": {"hdr-sess"}, "X-Claude-Code-Agent-Id": {"hdr-agent"}},
			payload: []byte(`{"metadata":{"agent_id":"ignored-agent"}}`),
		},
		{
			name:    "Codex-style payload with Session-Id header and no metadata",
			headers: http.Header{"Session-Id": {"codex-sess-1"}, "Thread-Id": {"codex-thread-1"}},
			payload: []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"do task"}],"tools":[{"type":"function","name":"bash"}]}`),
		},
		{
			name:    "Codex-style payload without headers and with body session_id",
			headers: nil,
			payload: []byte(`{"session_id":"body-sess-1","model":"gpt-5","input":[]}`),
		},
		{
			name:    "duplicate top-level metadata keys",
			headers: nil,
			payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"first\"}"},"metadata":{"user_id":"{\"session_id\":\"second\"}"}}`),
		},
		{
			name:    "payload with parent_session_id key outranking Claude metadata parent",
			headers: nil,
			payload: []byte(`{"parent_session_id":"parent-top","metadata":{"user_id":"{\"session_id\":\"child-sid\",\"parent_session_id\":\"parent-claude\"}"}}`),
		},
		{
			name:    "payload with neither Claude metadata nor explicit session",
			headers: nil,
			payload: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`),
		},
		{
			name:     "payload with caller scope metadata",
			headers:  http.Header{"Session-Id": {"scoped-sess"}},
			payload:  []byte(`{"model":"gpt-5"}`),
			metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "test-scope"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotOK := ExtractSessionInfo(tc.headers, tc.payload, tc.metadata)
			want, wantOK := legacyExtractSessionInfo(tc.headers, tc.payload, tc.metadata)

			if gotOK != wantOK {
				t.Fatalf("ok mismatch: got %v, want %v", gotOK, wantOK)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("session info mismatch:\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}
}
