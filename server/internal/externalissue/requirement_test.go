package externalissue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeRequirementSyncRequestAcceptsUnknownFieldsAndPreservesPrompt(t *testing.T) {
	raw := `{
		"id":"wu_decode_test",
		"title":"Requirement",
		"owner_id":"syndra-owner",
		"product_iteration_id":2262,
		"project_record_url":"https://example.com/record/2262",
		"development_role":"fullstack",
		"execution_prompt":"  keep prompt whitespace  ",
		"executor_id":"11111111-1111-4111-8111-111111111111",
		"lane_id":"lane_decode_test",
		"lane_type":"fullstack",
		"current_attempt":{"id":"att_decode_test","provider_run_id":"prun_decode_test"},
		"model":"ignored-model",
		"reasoning_effort":"xhigh",
		"unknown":{"nested":true}
	}`
	payload, err := DecodeRequirementSyncRequest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeRequirementSyncRequest: %v", err)
	}
	payload = normalizeRequirementSyncPayload(payload)
	if payload.ExecutionPrompt != "  keep prompt whitespace  " {
		t.Fatalf("execution_prompt = %q, want whitespace preserved", payload.ExecutionPrompt)
	}
	if len(payload.Model) == 0 || len(payload.ReasoningEffort) == 0 {
		t.Fatalf("ignored runtime fields were not detected: model=%q reasoning_effort=%q", payload.Model, payload.ReasoningEffort)
	}
	if _, err := validateRequirementSyncPayload(payload); err != nil {
		t.Fatalf("validateRequirementSyncPayload: %v", err)
	}
	content := buildRequirementIssueContent(payload)
	wantTitle := "[SYN:v1:syndra-flow-v1:wu_decode_test:att_decode_test] Requirement"
	if content.Title != wantTitle {
		t.Fatalf("assembled title = %q, want %q", content.Title, wantTitle)
	}
	wantDescription := "结构化需求源：\n" +
		"external.product_iteration_id=2262\n" +
		"project_record_url=https://example.com/record/2262\n" +
		"development_role=fullstack\n\n" +
		"  keep prompt whitespace  \n\n" +
		"---\n\n" +
		"[SYN:v1:syndra-flow-v1:wu_decode_test:att_decode_test]\n" +
		"dispatch_key=syndra-flow-v1:wu_decode_test:att_decode_test\n" +
		"lane_id=lane_decode_test\n" +
		"work_unit_id=wu_decode_test\n" +
		"external.lane_type=fullstack\n" +
		"external.provider_run_id=prun_decode_test"
	if content.Description != wantDescription {
		t.Fatalf("assembled description = %q, want %q", content.Description, wantDescription)
	}
	metadata := requirementIssueMetadata(payload, content)
	if metadata["dispatch_key"] != "syndra-flow-v1:wu_decode_test:att_decode_test" ||
		metadata["attempt_id"] != "att_decode_test" ||
		metadata["provider_run_id"] != "prun_decode_test" {
		t.Fatalf("trace metadata = %#v", metadata)
	}
	if _, ok := metadata["model"]; ok {
		t.Fatalf("model must not be written to issue metadata: %#v", metadata)
	}
	if _, ok := metadata["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort must not be written to issue metadata: %#v", metadata)
	}
}

func TestBuildRequirementIssueContentPrefersExplicitDispatchKey(t *testing.T) {
	payload := RequirementSyncPayload{
		ID:                 "wu_explicit_test",
		Title:              "Requirement",
		ProductIterationID: 2262,
		ProjectRecordURL:   "https://example.com/record/2262",
		ExecutionPrompt:    "Prompt",
		DispatchKey:        "syndra-flow-v1:explicit",
		CurrentAttempt:     json.RawMessage(`{"id":"att_ignored","dispatch_key":"syndra-flow-v1:attempt"}`),
	}
	content := buildRequirementIssueContent(payload)
	if content.DispatchKey != payload.DispatchKey {
		t.Fatalf("dispatch key = %q, want explicit %q", content.DispatchKey, payload.DispatchKey)
	}
	if content.Title != "[SYN:v1:syndra-flow-v1:explicit] Requirement" {
		t.Fatalf("title = %q", content.Title)
	}
}

func TestDecodeRequirementSyncRequestRejectsTrailingJSON(t *testing.T) {
	_, err := DecodeRequirementSyncRequest(strings.NewReader(`{} {}`))
	if err == nil {
		t.Fatal("DecodeRequirementSyncRequest accepted multiple JSON values")
	}
}

func TestValidateRequirementSyncPayloadRequiresMappedFields(t *testing.T) {
	base := RequirementSyncPayload{
		ID:                 "wu_validation_test",
		Title:              "Requirement",
		OwnerID:            "syndra-owner",
		ProductIterationID: 2262,
		ProjectRecordURL:   "https://example.com/record/2262",
		ExecutionPrompt:    "Prompt",
		ExecutorID:         "11111111-1111-4111-8111-111111111111",
	}
	tests := []struct {
		name string
		edit func(*RequirementSyncPayload)
		want error
	}{
		{name: "id", edit: func(p *RequirementSyncPayload) { p.ID = "" }, want: ErrMissingRecordID},
		{name: "title", edit: func(p *RequirementSyncPayload) { p.Title = "" }, want: ErrMissingTitle},
		{name: "execution prompt", edit: func(p *RequirementSyncPayload) { p.ExecutionPrompt = " \n\t " }, want: ErrMissingExecutionPrompt},
		{name: "executor", edit: func(p *RequirementSyncPayload) { p.ExecutorID = "" }, want: ErrMissingExecutorID},
		{name: "executor UUID", edit: func(p *RequirementSyncPayload) { p.ExecutorID = "not-a-uuid" }, want: ErrInvalidExecutorID},
		{name: "owner", edit: func(p *RequirementSyncPayload) { p.OwnerID = "" }, want: ErrMissingRequirementOwnerID},
		{name: "iteration", edit: func(p *RequirementSyncPayload) { p.ProductIterationID = 0 }, want: ErrMissingProductIterationID},
		{name: "record URL", edit: func(p *RequirementSyncPayload) { p.ProjectRecordURL = "" }, want: ErrMissingProjectRecordURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := base
			tt.edit(&payload)
			_, err := validateRequirementSyncPayload(payload)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
