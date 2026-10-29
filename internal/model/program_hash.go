package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ProgramAnswerSHA identifies the historical immutable answer contract. Server
// timestamps and independently authenticated audit provenance never alter its
// domain identity. Keep the field order and JSON tags stable: existing manifests
// and answer-created revisions refer to these exact bytes.
func ProgramAnswerSHA(answer QuestionAnswer) string {
	data, _ := json.Marshal(struct {
		ProgramID           string            `json:"program_id"`
		RevisionHash        string            `json:"revision_hash"`
		QuestionArtifactID  string            `json:"question_artifact_id"`
		QuestionArtifactSHA string            `json:"question_artifact_sha"`
		QuestionID          string            `json:"question_id"`
		Text                string            `json:"text"`
		Actor               string            `json:"actor"`
		SupportingRefs      []ProgramInputRef `json:"supporting_refs"`
		CreatedAt           time.Time         `json:"created_at"`
	}{answer.ProgramID, answer.RevisionHash, answer.QuestionArtifactID, answer.QuestionArtifactSHA, answer.QuestionID, answer.Text, answer.Actor, answer.SupportingRefs, time.Time{}})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
